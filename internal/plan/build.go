package plan

import (
	"fmt"
	"strings"

	"vitess.io/vitess/go/vt/sqlparser"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/policy"
)

// Plan is a built plan tree plus the classification verdicts every
// predicate received, keyed by its normalized "column op value" text.
type Plan struct {
	Root     Node
	verdicts map[string]Verdict

	// security is a pre-mutation snapshot of every security-origin
	// predicate Build injected. Only needed because tests simulate an
	// optimizer bug by mutating the tree after the fact (RemoveFilter) -
	// see IMPLEMENTATION_PLAN.md Cycle 3. Without it, AssertInvariant
	// would have nothing to check the mutated tree against but itself.
	security []securityPredicate
}

type securityPredicate struct {
	Pred    Predicate
	Verdict Verdict
}

func (p *Plan) VerdictFor(expr string) Verdict {
	return p.verdicts[normalizeExpr(expr)]
}

// PrimaryScan returns the first Scan in tree order - the only one, for a
// single-table plan. A join plan has one per side; use PrimaryJoin and
// ScanIn(side.Root) instead, which is the only way to get the right one once
// two aliases can name the same table.
//
// Derived from the tree rather than a parallel map. The map it replaced was
// keyed by table name, so a self-join silently kept one side's Scan and
// dropped the other's, and PrimaryScan read it by ranging - which Go
// randomises, so with more than one entry the answer was not even stable.
// The tree is the single source of truth; nothing can now disagree with it.
func (p *Plan) PrimaryScan() *Scan {
	return ScanIn(p.Root)
}

// PrimaryJoin returns the plan's Join node, or nil for a single-table plan.
func (p *Plan) PrimaryJoin() *Join {
	var found *Join
	walk(p.Root, func(n Node) {
		if found != nil {
			return
		}
		if j, ok := n.(*Join); ok {
			found = j
		}
	})
	return found
}

// ScanIn returns the Scan node within n's subtree - how callers find one
// side's underlying table once PrimaryJoin has split the tree in two.
func ScanIn(n Node) *Scan {
	var found *Scan
	walk(n, func(nd Node) {
		if found != nil {
			return
		}
		if s, ok := nd.(*Scan); ok {
			found = s
		}
	})
	return found
}

// FiltersIn returns every Filter node within n's subtree - scoped to one
// side of a join, unlike Filters which walks the whole plan.
func FiltersIn(n Node) []*Filter {
	var out []*Filter
	walk(n, func(nd Node) {
		if f, ok := nd.(*Filter); ok {
			out = append(out, f)
		}
	})
	return out
}

// Filters returns every Filter node in the tree, pushed and local alike -
// what exec needs to know both what to send to the connector and what to
// re-check afterward.
func (p *Plan) Filters() []*Filter {
	return FiltersIn(p.Root)
}

// OutputColumns returns the final output projection - what the top
// Project node trims the over-projected scan back down to.
func (p *Plan) OutputColumns() []ProjectCol {
	if proj, ok := p.Root.(*Project); ok {
		return proj.Cols
	}
	scan := p.PrimaryScan()
	if scan == nil {
		return nil
	}
	cols := make([]ProjectCol, len(scan.Project))
	for i, c := range scan.Project {
		cols[i] = ProjectCol{Name: c}
	}
	return cols
}

func normalizeExpr(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

var parser = sqlparser.NewTestParser()

// Build parses sql (v1 surface: single table or one equi-join, conjunctive
// WHERE, simple column projection) and classifies every predicate - from
// the query text and from residuals - against cat's capability profile for
// each table. masks declares which output columns get CLS masking applied.
func Build(sql string, cat *catalog.Catalog, residuals policy.Residuals, masks policy.Masks) (*Plan, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("plan: parse: %w", err)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok || len(sel.From) != 1 {
		return nil, ErrUnsupportedStatement
	}

	if je, ok := sel.From[0].(*sqlparser.JoinTableExpr); ok {
		return buildJoin(sel, je, cat, residuals, masks)
	}

	table, err := tableName(sel.From[0])
	if err != nil {
		return nil, err
	}

	projectCols, err := projection(sel.SelectExprs)
	if err != nil {
		return nil, err
	}

	var wheres []whereCond
	if sel.Where != nil {
		conjuncts, err := splitConjuncts(sel.Where.Expr)
		if err != nil {
			return nil, err
		}
		for i, c := range conjuncts {
			col, op, value, err := comparisonParts(c)
			if err != nil {
				return nil, err
			}
			wheres = append(wheres, whereCond{Col: col, Op: op, Literal: value, Param: paramPlaceholder(i)})
		}
	}

	var maskCols []string
	var outCols []ProjectCol
	if len(projectCols) > 0 {
		outCols = make([]ProjectCol, len(projectCols))
		for i, c := range projectCols {
			outCols[i] = ProjectCol{Name: c}
			for _, m := range masks {
				if m.Table == table && m.Column == c {
					fn := m.Fn
					outCols[i].Mask = &fn
					maskCols = append(maskCols, c)
				}
			}
		}
	}

	if err := checkMaskingSupported(cat, table, maskCols); err != nil {
		return nil, err
	}

	root, _, verdicts, security, err := buildSideTree(table, wheres, residuals, cat, "", projectCols, maskCols)
	if err != nil {
		return nil, err
	}

	if len(outCols) > 0 {
		root = &Project{Child: root, Cols: outCols}
	}

	return &Plan{
		Root:     root,
		verdicts: verdicts,
		security: security,
	}, nil
}

// whereCond is one WHERE-clause conjunct, already stripped of its table
// alias (single-table Build never had qualifiers; buildJoin routes each
// conjunct to its owning side before this point). Literal is the actual
// text the query used (e.g. "'open'") - kept only so VerdictFor can be
// looked up by the text a caller actually wrote. Param is the
// "$param.N" placeholder stored in the Filter/Predicate instead: a cached
// Plan is shared across every future request of the same shape (ADR-003),
// so the literal itself must never be baked in - it's re-extracted fresh
// from each request's own SQL by ExtractParams and bound at exec.Run time,
// the same way $principal.<attr> values already were (Cycle 7). Baking
// Literal in directly here was exactly the bug this indirection fixes:
// every cache hit after the first silently kept re-using whichever
// request's literal built the plan.
type whereCond struct {
	Col, Op, Literal, Param string
}

// buildSideTree builds one Scan plus its stack of Filter nodes - the whole
// tree for a single-table plan, or one side of a join. joinKeyCol is
// always fetched even if it's neither an output column nor a predicate
// column (the semi-join needs it after fetch to key the in-memory match);
// pass "" for a non-join build.
// buildSideTreeMulti is buildSideTree for a side that may carry more than
// one join key. A middle table in an N-way chain is both a probe (for the
// link that reaches it) and a build side (for the link that leaves it), so
// every key column it participates in has to survive over-projection.
func buildSideTreeMulti(table string, wheres []whereCond, residuals policy.Residuals, cat *catalog.Catalog, joinKeyCols, outputCols, maskCols []string) (Node, *Scan, map[string]Verdict, []securityPredicate, error) {
	root, scan, v, sec, err := buildSideTree(table, wheres, residuals, cat, "", outputCols, maskCols)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, k := range joinKeyCols {
		scan.Project = appendUnique(scan.Project, k)
	}
	return root, scan, v, sec, nil
}

func buildSideTree(table string, wheres []whereCond, residuals policy.Residuals, cat *catalog.Catalog, joinKeyCol string, outputCols, maskCols []string) (Node, *Scan, map[string]Verdict, []securityPredicate, error) {
	scan := &Scan{Table: table}
	verdicts := make(map[string]Verdict)
	var root Node = scan

	// predicateCols collects every predicate's column, pushed or local, in
	// first-seen order. A PUSHED_ENFORCED predicate's column still needs
	// fetching: the verification filter (Cycle 6) re-checks it locally
	// after fetch, so over-projection applies to it too.
	var predicateCols []string
	seenCol := make(map[string]bool)

	// verdictValue is the text VerdictFor's caller would naturally write
	// (the actual literal, or $principal.<attr> for a residual);
	// storedValue is what's kept in the Filter/Predicate for exec to
	// resolve later - a $param.N placeholder for a user literal, or the
	// unchanged $principal.<attr> text for a residual, which was already
	// resolved lazily before this fix existed.
	applyPredicate := func(col, op, verdictValue, storedValue string, origin Origin) Verdict {
		exprText := normalizeExpr(col + " " + op + " " + verdictValue)
		v := classify(cat, table, col, op)
		verdicts[exprText] = v
		if v.Site == Pushed {
			scan.Pushed = append(scan.Pushed, col)
		}
		if !seenCol[col] {
			seenCol[col] = true
			predicateCols = append(predicateCols, col)
		}
		root = &Filter{
			Child: root,
			Pred:  Predicate{Column: col, Op: op, Value: storedValue, Origin: origin},
			Site:  v.Site,
		}
		return v
	}

	for _, w := range wheres {
		applyPredicate(w.Col, w.Op, w.Literal, w.Param, UserOrigin)
	}

	var security []securityPredicate
	for _, r := range residuals {
		if r.Table != table {
			continue
		}
		col, op, value, err := residualParts(r.Expr)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		v := applyPredicate(col, op, value, value, SecurityOrigin)
		security = append(security, securityPredicate{
			Pred:    Predicate{Column: col, Op: op, Value: value, Origin: SecurityOrigin},
			Verdict: v,
		})
	}

	var joinKeyCols []string
	if joinKeyCol != "" {
		joinKeyCols = []string{joinKeyCol}
	}

	// The scan projects the union of the output columns, every predicate
	// column (pushed or local), mask columns, and the join key (if any) -
	// then the top Project trims back.
	scan.Project = dedupUnion(outputCols, predicateCols, maskCols, joinKeyCols)

	return root, scan, verdicts, security, nil
}

// checkMaskingSupported asserts the catalog agrees table's connector can't
// mask itself, when a mask is about to be applied locally. v1 has no
// pushed-masking implementation, so a catalog claiming otherwise is a
// configuration error to catch now, not a capability to silently ignore.
func checkMaskingSupported(cat *catalog.Catalog, table string, maskCols []string) error {
	if len(maskCols) == 0 {
		return nil
	}
	t, ok := cat.Table(table)
	if !ok || t.Masking != catalog.MaskingUnsupported {
		return fmt.Errorf("%w: %s declares masking %q", ErrMaskingUnsupported, table, t.Masking)
	}
	return nil
}

// buildJoin handles the one join shape v1 supports: two aliased tables,
// one equi-join ON condition, alias-qualified SELECT and WHERE columns. It
// mirrors Build's single-table logic per side (buildSideTree), then
// combines both sides under one Join node.
// flattenJoins walks the left-deep tree sqlparser builds for
// `A JOIN B ON .. JOIN C ON ..` - ((A JOIN B) JOIN C) - collecting leaf
// tables in FROM order alongside the ON condition that attaches each one.
// It returns one more table than condition, which is what makes N-1 links.
func flattenJoins(e sqlparser.TableExpr) ([]*sqlparser.AliasedTableExpr, []*sqlparser.JoinCondition, error) {
	switch t := e.(type) {
	case *sqlparser.AliasedTableExpr:
		return []*sqlparser.AliasedTableExpr{t}, nil, nil
	case *sqlparser.JoinTableExpr:
		if t.Join != sqlparser.NormalJoinType {
			return nil, nil, fmt.Errorf("%w: only INNER JOIN is supported (got %q); an outer join needs null-extending semantics the semi-join rewrite cannot express",
				ErrUnsupportedStatement, t.Join.ToString())
		}
		if t.Condition == nil || t.Condition.On == nil {
			return nil, nil, fmt.Errorf("%w: join requires an ON condition", ErrUnsupportedStatement)
		}
		tables, conds, err := flattenJoins(t.LeftExpr)
		if err != nil {
			return nil, nil, err
		}
		right, ok := t.RightExpr.(*sqlparser.AliasedTableExpr)
		if !ok {
			return nil, nil, fmt.Errorf("%w: the right side of a join must be a table, not a nested join", ErrUnsupportedStatement)
		}
		return append(tables, right), append(conds, t.Condition), nil
	default:
		return nil, nil, ErrUnsupportedStatement
	}
}

func appendUnique(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(ss, s)
}

// buildJoin handles an N-way inner equi-join: N aliased tables, N-1 ON
// conditions, alias-qualified SELECT and WHERE columns. It mirrors Build's
// single-table logic per side (buildSideTreeMulti), then combines every side
// under one Join node.
//
// Outer joins are rejected in flattenJoins rather than downgraded.
// LEFT/RIGHT/NATURAL parse into the same JoinTableExpr, so without that check
// they would run as inner joins and silently return a *smaller* result than
// asked for, with a 200. They also defeat the semi-join rewrite outright:
// pushing a side's keys as an IN-list is precisely what makes unmatched keys
// invisible.
func buildJoin(sel *sqlparser.Select, je *sqlparser.JoinTableExpr, cat *catalog.Catalog, residuals policy.Residuals, masks policy.Masks) (*Plan, error) {
	ates, conds, err := flattenJoins(je)
	if err != nil {
		return nil, err
	}
	n := len(ates)

	aliases := make([]string, n)
	tables := make([]string, n)
	byAlias := make(map[string]int, n)
	for i, ate := range ates {
		tbl, err := tableName(ate)
		if err != nil {
			return nil, err
		}
		alias := ate.As.String()
		if alias == "" {
			alias = tbl
		}
		if _, dup := byAlias[alias]; dup {
			return nil, fmt.Errorf("%w: alias %q used twice; every join side needs a distinct alias", ErrUnsupportedStatement, alias)
		}
		aliases[i], tables[i], byAlias[alias] = alias, tbl, i
	}

	// Each ON condition attaches ates[i+1] to a side already in the join.
	// Requiring that side to be strictly earlier in FROM order is what keeps
	// the cascade fetchable: the IN-list pushed into a probe is always built
	// from rows already retrieved.
	links := make([]Link, 0, n-1)
	keyCols := make([][]string, n) // join keys per side, for over-projection
	for i, cond := range conds {
		to := i + 1
		aAlias, aCol, bAlias, bCol, err := equiJoinKeysAny(cond.On, byAlias)
		if err != nil {
			return nil, err
		}
		var from int
		var fromCol, toCol string
		switch {
		case byAlias[bAlias] == to:
			from, fromCol, toCol = byAlias[aAlias], aCol, bCol
		case byAlias[aAlias] == to:
			from, fromCol, toCol = byAlias[bAlias], bCol, aCol
		default:
			return nil, fmt.Errorf("%w: the ON condition joining %q must reference %q", ErrUnsupportedStatement, aliases[to], aliases[to])
		}
		if from >= to {
			return nil, fmt.Errorf("%w: %q joins to a table appearing later in the FROM clause; reorder so each join references an earlier table",
				ErrUnsupportedStatement, aliases[to])
		}
		probe, ok := cat.Table(tables[to])
		if !ok || !probe.JoinKeyInList {
			return nil, fmt.Errorf("%w: %s does not declare join_key_in_list - a semi-join probe side must accept a chunked IN-list",
				ErrUnsupportedStatement, tables[to])
		}
		links = append(links, Link{From: from, FromCol: fromCol, To: to, ToCol: toCol, MaxInList: probe.MaxInList})
		keyCols[from] = appendUnique(keyCols[from], fromCol)
		keyCols[to] = appendUnique(keyCols[to], toCol)
	}

	projCols, err := projectionQualified(sel.SelectExprs)
	if err != nil {
		return nil, err
	}
	perSideProject := make([][]string, n)
	outCols := make([]ProjectCol, len(projCols))
	for i, c := range projCols {
		si, ok := byAlias[c.Alias]
		if !ok {
			return nil, fmt.Errorf("%w: unknown table alias %q", ErrUnsupportedStatement, c.Alias)
		}
		perSideProject[si] = appendUnique(perSideProject[si], c.Column)
		outCols[i] = ProjectCol{Name: c.Column, Side: aliases[si]}
		for _, m := range masks {
			if m.Table == tables[si] && m.Column == c.Column {
				fn := m.Fn
				outCols[i].Mask = &fn
			}
		}
	}

	perSideWhere := make([][]whereCond, n)
	if sel.Where != nil {
		conjuncts, err := splitConjuncts(sel.Where.Expr)
		if err != nil {
			return nil, err
		}
		// Param placeholders are numbered by i, the conjunct's position in
		// the ORIGINAL (unsplit) list - not its position after being bucketed
		// by alias - so it lines up with ExtractParams, which re-parses the
		// same original list without ever routing by alias.
		for i, c := range conjuncts {
			alias, col, op, value, err := comparisonPartsQualified(c)
			if err != nil {
				return nil, err
			}
			si, ok := byAlias[alias]
			if !ok {
				return nil, fmt.Errorf("%w: unknown table alias %q", ErrUnsupportedStatement, alias)
			}
			perSideWhere[si] = append(perSideWhere[si], whereCond{Col: col, Op: op, Literal: value, Param: paramPlaceholder(i)})
		}
	}

	sides := make([]JoinSide, n)
	verdicts := map[string]Verdict{}
	var security []securityPredicate
	for i := 0; i < n; i++ {
		maskCols := maskColsFor(tables[i], masks)
		if err := checkMaskingSupported(cat, tables[i], maskCols); err != nil {
			return nil, err
		}
		root, _, v, sec, err := buildSideTreeMulti(tables[i], perSideWhere[i], residuals, cat, keyCols[i], perSideProject[i], maskCols)
		if err != nil {
			return nil, err
		}
		// The Scan lives in root's subtree and is reached with ScanIn. There
		// is deliberately no side table of scans: two aliases can name one
		// table, so any map keyed by table name would drop a side.
		sides[i] = JoinSide{Alias: aliases[i], Table: tables[i], Root: root}
		for k, vv := range v {
			verdicts[k] = vv
		}
		security = append(security, sec...)
	}

	join := &Join{Sides: sides, Links: links}
	var root Node = join
	if len(outCols) > 0 {
		root = &Project{Child: join, Cols: outCols}
	}
	return &Plan{Root: root, verdicts: verdicts, security: security}, nil
}

// ParamPrefix is the marker exec's resolveValue recognizes for a
// WHERE-clause literal that must be re-resolved per call - see whereCond's
// doc comment for why the literal itself is never stored in the Plan.
// Exported so exec can parse exactly the prefix this package writes,
// rather than each side keeping its own copy of the string in sync by
// hand.
const ParamPrefix = "$param."

func paramPlaceholder(i int) string {
	return fmt.Sprintf("%s%d", ParamPrefix, i)
}

// ExtractParams re-parses sql's WHERE-clause literal values, in the exact
// order Build assigns them $param.N placeholders (conjunct position in the
// unsplit list - see paramPlaceholder and buildJoin's ordering note).
// Called at exec time against the CURRENT request's own SQL text - not the
// (possibly different) request that built and cached the Plan - so a plan
// cache hit never serves a stale literal. Residual ($principal.<attr>)
// predicates aren't part of this: they come from policy, not the query
// text, and were already resolved lazily before this existed.
func ExtractParams(sql string) ([]string, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("plan: parse: %w", err)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok || len(sel.From) != 1 {
		return nil, ErrUnsupportedStatement
	}
	if sel.Where == nil {
		return nil, nil
	}

	conjuncts, err := splitConjuncts(sel.Where.Expr)
	if err != nil {
		return nil, err
	}

	params := make([]string, len(conjuncts))
	for i, c := range conjuncts {
		cmp, ok := c.(*sqlparser.ComparisonExpr)
		if !ok {
			return nil, fmt.Errorf("%w: only simple comparisons supported", ErrUnsupportedPredicate)
		}
		lit, ok := cmp.Right.(*sqlparser.Literal)
		if !ok {
			return nil, fmt.Errorf("%w: right side must be a literal", ErrUnsupportedPredicate)
		}
		value, err := literalText(lit)
		if err != nil {
			return nil, err
		}
		params[i] = value
	}
	return params, nil
}

func maskColsFor(table string, masks policy.Masks) []string {
	var out []string
	for _, m := range masks {
		if m.Table == table {
			out = append(out, m.Column)
		}
	}
	return out
}

func dedupUnion(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, c := range list {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// ParseTables extracts every table sql queries (one for a single-table
// SELECT, two - build then probe - for a join), without building a plan.
// Orchestration code needs the tables before it can ask a policy.Provider
// for residuals or run L1 object-authz, which Build then takes as
// arguments.
func ParseTables(sql string) ([]string, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("plan: parse: %w", err)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok || len(sel.From) != 1 {
		return nil, ErrUnsupportedStatement
	}

	if je, ok := sel.From[0].(*sqlparser.JoinTableExpr); ok {
		// Every side, not just the outermost two. This feeds layer-1 object
		// authz, so returning a subset would admit a denied table: the
		// caller checks only what it is given. Erroring is the safe failure
		// and a partial list is not - which is why flattenJoins is shared
		// with buildJoin rather than re-derived here.
		ates, _, err := flattenJoins(je)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(ates))
		for _, ate := range ates {
			t, err := tableName(ate)
			if err != nil {
				return nil, err
			}
			out = appendUnique(out, t)
		}
		return out, nil
	}

	table, err := tableName(sel.From[0])
	if err != nil {
		return nil, err
	}
	return []string{table}, nil
}

// Shape returns sql normalized with every literal value replaced by a
// placeholder - the plan cache key's sql_shape component (DESIGN.md
// ADR-003). Two queries differing only in a WHERE-clause value (e.g.
// status='open' vs status='closed') must produce the same shape, so a
// cached plan (with a parameter slot, not the value baked in) can serve
// both.
func Shape(sql string) (string, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("plan: parse: %w", err)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok || len(sel.From) != 1 {
		return "", ErrUnsupportedStatement
	}

	if je, ok := sel.From[0].(*sqlparser.JoinTableExpr); ok {
		return joinShape(sel, je)
	}

	table, err := tableName(sel.From[0])
	if err != nil {
		return "", err
	}
	projectCols, err := projection(sel.SelectExprs)
	if err != nil {
		return "", err
	}

	var predShapes []string
	if sel.Where != nil {
		conjuncts, err := splitConjuncts(sel.Where.Expr)
		if err != nil {
			return "", err
		}
		for _, c := range conjuncts {
			col, op, _, err := comparisonParts(c)
			if err != nil {
				return "", err
			}
			predShapes = append(predShapes, col+" "+op+" ?")
		}
	}

	shape := "SELECT " + strings.Join(projectCols, ",") + " FROM " + table
	if len(predShapes) > 0 {
		shape += " WHERE " + strings.Join(predShapes, " AND ")
	}
	return shape, nil
}

func joinShape(sel *sqlparser.Select, je *sqlparser.JoinTableExpr) (string, error) {
	ates, conds, err := flattenJoins(je)
	if err != nil {
		return "", err
	}
	names := make([]string, len(ates))
	for i, ate := range ates {
		t, err := tableName(ate)
		if err != nil {
			return "", err
		}
		alias := ate.As.String()
		if alias == "" {
			alias = t
		}
		names[i] = t + " " + alias
	}

	projCols, err := projectionQualified(sel.SelectExprs)
	if err != nil {
		return "", err
	}
	parts := make([]string, len(projCols))
	for i, c := range projCols {
		parts[i] = c.Alias + "." + c.Column
	}

	var predShapes []string
	if sel.Where != nil {
		conjuncts, err := splitConjuncts(sel.Where.Expr)
		if err != nil {
			return "", err
		}
		for _, cj := range conjuncts {
			alias, col, op, _, err := comparisonPartsQualified(cj)
			if err != nil {
				return "", err
			}
			predShapes = append(predShapes, alias+"."+col+" "+op+" ?")
		}
	}

	onShapes := make([]string, len(conds))
	for i, cond := range conds {
		onShapes[i] = sqlparser.String(cond.On)
	}

	shape := "SELECT " + strings.Join(parts, ",") + " FROM " + strings.Join(names, " JOIN ") +
		" ON " + strings.Join(onShapes, " AND ")
	if len(predShapes) > 0 {
		shape += " WHERE " + strings.Join(predShapes, " AND ")
	}
	return shape, nil
}

// classify is the capability walk: ENFORCED -> push, ADVISORY -> retain
// locally, unknown column/op -> retain locally as Unsupported.
//
// Deliberate simplification: this applies the ADVISORY-never-pushes rule
// to every predicate regardless of Origin. ADR-002 only requires that for
// SECURITY predicates (over-filtering risk - see DESIGN.md ADR-002, "Why
// not push security predicates to ADVISORY connectors"); for USER
// predicates it says ADVISORY should still be pushed as a volume
// optimization, with the local filter kept for correctness regardless.
// Not fixed here: safe (over-conservative, costs bandwidth not
// correctness) and no current fixture exercises a user-origin predicate
// against an ADVISORY column. See IMPLEMENTATION_PLAN.md Cycle 2.
func classify(cat *catalog.Catalog, table, column, op string) Verdict {
	capa, ok := cat.Capability(table, column)
	if !ok || !capa.SupportsOp(op) {
		return Verdict{Disposition: Unsupported, Site: Local}
	}
	if capa.Enforcement == catalog.Enforced {
		return Verdict{Disposition: PushedEnforced, Site: Pushed}
	}
	return Verdict{Disposition: Residual, Site: Local}
}

func tableName(te sqlparser.TableExpr) (string, error) {
	ate, ok := te.(*sqlparser.AliasedTableExpr)
	if !ok {
		return "", ErrUnsupportedStatement
	}
	tn, ok := ate.Expr.(sqlparser.TableName)
	if !ok {
		return "", ErrUnsupportedStatement
	}
	if tn.Qualifier.IsEmpty() {
		return tn.Name.String(), nil
	}
	return tn.Qualifier.String() + "." + tn.Name.String(), nil
}

func projection(se *sqlparser.SelectExprs) ([]string, error) {
	if se == nil {
		return nil, nil
	}
	cols := make([]string, 0, len(se.Exprs))
	for _, expr := range se.Exprs {
		ae, ok := expr.(*sqlparser.AliasedExpr)
		if !ok {
			return nil, fmt.Errorf("%w: only simple column projections supported", ErrUnsupportedStatement)
		}
		col, ok := ae.Expr.(*sqlparser.ColName)
		if !ok {
			return nil, fmt.Errorf("%w: only simple column projections supported", ErrUnsupportedStatement)
		}
		cols = append(cols, col.Name.String())
	}
	return cols, nil
}

// qualifiedCol is one SELECT-list or WHERE column qualified by its table
// alias - required for a join, where an unqualified column name would be
// ambiguous between the two tables.
type qualifiedCol struct {
	Alias  string
	Column string
}

func projectionQualified(se *sqlparser.SelectExprs) ([]qualifiedCol, error) {
	if se == nil {
		return nil, nil
	}
	cols := make([]qualifiedCol, 0, len(se.Exprs))
	for _, expr := range se.Exprs {
		ae, ok := expr.(*sqlparser.AliasedExpr)
		if !ok {
			return nil, fmt.Errorf("%w: only simple column projections supported", ErrUnsupportedStatement)
		}
		cn, ok := ae.Expr.(*sqlparser.ColName)
		if !ok {
			return nil, fmt.Errorf("%w: only simple column projections supported", ErrUnsupportedStatement)
		}
		if cn.Qualifier.Name.IsEmpty() {
			return nil, fmt.Errorf("%w: a joined query requires alias-qualified columns (e.g. a.name)", ErrUnsupportedStatement)
		}
		cols = append(cols, qualifiedCol{Alias: cn.Qualifier.Name.String(), Column: cn.Name.String()})
	}
	return cols, nil
}

func splitConjuncts(e sqlparser.Expr) ([]sqlparser.Expr, error) {
	if and, ok := e.(*sqlparser.AndExpr); ok {
		left, err := splitConjuncts(and.Left)
		if err != nil {
			return nil, err
		}
		right, err := splitConjuncts(and.Right)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	return []sqlparser.Expr{e}, nil
}

func comparisonParts(e sqlparser.Expr) (col, op, value string, err error) {
	cmp, ok := e.(*sqlparser.ComparisonExpr)
	if !ok {
		return "", "", "", fmt.Errorf("%w: only simple comparisons supported", ErrUnsupportedPredicate)
	}
	cn, ok := cmp.Left.(*sqlparser.ColName)
	if !ok {
		return "", "", "", fmt.Errorf("%w: left side must be a column", ErrUnsupportedPredicate)
	}
	lit, ok := cmp.Right.(*sqlparser.Literal)
	if !ok {
		return "", "", "", fmt.Errorf("%w: right side must be a literal", ErrUnsupportedPredicate)
	}
	value, err = literalText(lit)
	if err != nil {
		return "", "", "", err
	}
	return cn.Name.String(), cmp.Operator.ToString(), value, nil
}

// comparisonPartsQualified is comparisonParts for a joined query's WHERE
// clause, where the column must be alias-qualified (t.status = 'open')
// since an unqualified name would be ambiguous between the two tables.
func comparisonPartsQualified(e sqlparser.Expr) (alias, col, op, value string, err error) {
	cmp, ok := e.(*sqlparser.ComparisonExpr)
	if !ok {
		return "", "", "", "", fmt.Errorf("%w: only simple comparisons supported", ErrUnsupportedPredicate)
	}
	cn, ok := cmp.Left.(*sqlparser.ColName)
	if !ok {
		return "", "", "", "", fmt.Errorf("%w: left side must be a column", ErrUnsupportedPredicate)
	}
	lit, ok := cmp.Right.(*sqlparser.Literal)
	if !ok {
		return "", "", "", "", fmt.Errorf("%w: right side must be a literal", ErrUnsupportedPredicate)
	}
	value, err = literalText(lit)
	if err != nil {
		return "", "", "", "", err
	}
	if cn.Qualifier.Name.IsEmpty() {
		return "", "", "", "", fmt.Errorf("%w: a joined query requires alias-qualified predicate columns", ErrUnsupportedPredicate)
	}
	return cn.Qualifier.Name.String(), cn.Name.String(), cmp.Operator.ToString(), value, nil
}

// equiJoinKeys extracts the join key column names from an ON condition,
// returning (leftTableCol, rightTableCol) regardless of which side each
// one appears on in the source text - "t.org = a.ext" and "a.ext = t.org"
// resolve to the same pair.
// equiJoinKeysAny returns both sides of a column-to-column equality as
// (alias, column) pairs, leaving orientation to the caller - an N-way join
// discovers which side is already fetched rather than assuming left/right.
func equiJoinKeysAny(e sqlparser.Expr, known map[string]int) (aAlias, aCol, bAlias, bCol string, err error) {
	cmp, ok := e.(*sqlparser.ComparisonExpr)
	if !ok || cmp.Operator != sqlparser.EqualOp {
		return "", "", "", "", fmt.Errorf("%w: join condition must be a single column-to-column equality", ErrUnsupportedStatement)
	}
	lcn, lok := cmp.Left.(*sqlparser.ColName)
	rcn, rok := cmp.Right.(*sqlparser.ColName)
	if !lok || !rok {
		return "", "", "", "", fmt.Errorf("%w: join condition must compare two columns", ErrUnsupportedStatement)
	}
	la, ra := lcn.Qualifier.Name.String(), rcn.Qualifier.Name.String()
	if _, ok := known[la]; !ok {
		return "", "", "", "", fmt.Errorf("%w: unknown table alias %q in join condition", ErrUnsupportedStatement, la)
	}
	if _, ok := known[ra]; !ok {
		return "", "", "", "", fmt.Errorf("%w: unknown table alias %q in join condition", ErrUnsupportedStatement, ra)
	}
	return la, lcn.Name.String(), ra, rcn.Name.String(), nil
}

func equiJoinKeys(e sqlparser.Expr, leftAlias, rightAlias string) (leftCol, rightCol string, err error) {
	cmp, ok := e.(*sqlparser.ComparisonExpr)
	if !ok || cmp.Operator != sqlparser.EqualOp {
		return "", "", fmt.Errorf("%w: join condition must be a single column-to-column equality", ErrUnsupportedStatement)
	}
	lcn, lok := cmp.Left.(*sqlparser.ColName)
	rcn, rok := cmp.Right.(*sqlparser.ColName)
	if !lok || !rok {
		return "", "", fmt.Errorf("%w: join condition must compare two columns", ErrUnsupportedStatement)
	}
	la, ra := lcn.Qualifier.Name.String(), rcn.Qualifier.Name.String()
	switch {
	case la == leftAlias && ra == rightAlias:
		return lcn.Name.String(), rcn.Name.String(), nil
	case la == rightAlias && ra == leftAlias:
		return rcn.Name.String(), lcn.Name.String(), nil
	default:
		return "", "", fmt.Errorf("%w: join condition must reference both joined tables", ErrUnsupportedStatement)
	}
}

func literalText(lit *sqlparser.Literal) (string, error) {
	switch lit.Type {
	case sqlparser.StrVal:
		return "'" + lit.Val + "'", nil
	case sqlparser.IntVal:
		return lit.Val, nil
	default:
		return "", fmt.Errorf("%w: unsupported literal type", ErrUnsupportedPredicate)
	}
}

func residualParts(expr string) (col, op, value string, err error) {
	parts := strings.Fields(expr)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("%w: residual expr must be 'column op value', got %q", ErrUnsupportedPredicate, expr)
	}
	return parts[0], parts[1], parts[2], nil
}

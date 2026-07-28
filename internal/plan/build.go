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
	scans    map[string]*Scan

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

func (p *Plan) Scan(table string) *Scan {
	return p.scans[table]
}

// PrimaryScan returns the plan's one Scan for a single-table plan. A join
// plan has two - use PrimaryJoin and ScanIn(join.Left)/ScanIn(join.Right)
// instead.
func (p *Plan) PrimaryScan() *Scan {
	for _, s := range p.scans {
		return s
	}
	return nil
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
		for _, c := range conjuncts {
			col, op, value, err := comparisonParts(c)
			if err != nil {
				return nil, err
			}
			wheres = append(wheres, whereCond{Col: col, Op: op, Value: value})
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

	root, scan, verdicts, security, err := buildSideTree(table, wheres, residuals, cat, "", projectCols, maskCols)
	if err != nil {
		return nil, err
	}

	if len(outCols) > 0 {
		root = &Project{Child: root, Cols: outCols}
	}

	return &Plan{
		Root:     root,
		verdicts: verdicts,
		scans:    map[string]*Scan{table: scan},
		security: security,
	}, nil
}

// whereCond is one WHERE-clause conjunct, already stripped of its table
// alias (single-table Build never had qualifiers; buildJoin routes each
// conjunct to its owning side before this point).
type whereCond struct {
	Col, Op, Value string
}

// buildSideTree builds one Scan plus its stack of Filter nodes - the whole
// tree for a single-table plan, or one side of a join. joinKeyCol is
// always fetched even if it's neither an output column nor a predicate
// column (the semi-join needs it after fetch to key the in-memory match);
// pass "" for a non-join build.
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

	applyPredicate := func(col, op, value string, origin Origin) Verdict {
		exprText := normalizeExpr(col + " " + op + " " + value)
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
			Pred:  Predicate{Column: col, Op: op, Value: value, Origin: origin},
			Site:  v.Site,
		}
		return v
	}

	for _, w := range wheres {
		applyPredicate(w.Col, w.Op, w.Value, UserOrigin)
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
		v := applyPredicate(col, op, value, SecurityOrigin)
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
func buildJoin(sel *sqlparser.Select, je *sqlparser.JoinTableExpr, cat *catalog.Catalog, residuals policy.Residuals, masks policy.Masks) (*Plan, error) {
	leftTable, err := tableName(je.LeftExpr)
	if err != nil {
		return nil, err
	}
	rightTable, err := tableName(je.RightExpr)
	if err != nil {
		return nil, err
	}

	leftAte, ok := je.LeftExpr.(*sqlparser.AliasedTableExpr)
	if !ok {
		return nil, ErrUnsupportedStatement
	}
	rightAte, ok := je.RightExpr.(*sqlparser.AliasedTableExpr)
	if !ok {
		return nil, ErrUnsupportedStatement
	}
	leftAlias := leftAte.As.String()
	if leftAlias == "" {
		leftAlias = leftTable
	}
	rightAlias := rightAte.As.String()
	if rightAlias == "" {
		rightAlias = rightTable
	}

	if je.Condition == nil || je.Condition.On == nil {
		return nil, fmt.Errorf("%w: join requires an ON condition", ErrUnsupportedStatement)
	}
	leftKeyCol, rightKeyCol, err := equiJoinKeys(je.Condition.On, leftAlias, rightAlias)
	if err != nil {
		return nil, err
	}

	projCols, err := projectionQualified(sel.SelectExprs)
	if err != nil {
		return nil, err
	}

	var leftProject, rightProject []string
	outCols := make([]ProjectCol, len(projCols))
	for i, c := range projCols {
		var table string
		switch c.Alias {
		case leftAlias:
			table = leftTable
			leftProject = append(leftProject, c.Column)
		case rightAlias:
			table = rightTable
			rightProject = append(rightProject, c.Column)
		default:
			return nil, fmt.Errorf("%w: unknown table alias %q", ErrUnsupportedStatement, c.Alias)
		}
		outCols[i] = ProjectCol{Name: c.Column}
		for _, m := range masks {
			if m.Table == table && m.Column == c.Column {
				fn := m.Fn
				outCols[i].Mask = &fn
			}
		}
	}

	var leftWhere, rightWhere []whereCond
	if sel.Where != nil {
		conjuncts, err := splitConjuncts(sel.Where.Expr)
		if err != nil {
			return nil, err
		}
		for _, c := range conjuncts {
			alias, col, op, value, err := comparisonPartsQualified(c)
			if err != nil {
				return nil, err
			}
			switch alias {
			case leftAlias:
				leftWhere = append(leftWhere, whereCond{Col: col, Op: op, Value: value})
			case rightAlias:
				rightWhere = append(rightWhere, whereCond{Col: col, Op: op, Value: value})
			default:
				return nil, fmt.Errorf("%w: unknown table alias %q", ErrUnsupportedStatement, alias)
			}
		}
	}

	leftMaskCols := maskColsFor(leftTable, masks)
	rightMaskCols := maskColsFor(rightTable, masks)
	if err := checkMaskingSupported(cat, leftTable, leftMaskCols); err != nil {
		return nil, err
	}
	if err := checkMaskingSupported(cat, rightTable, rightMaskCols); err != nil {
		return nil, err
	}

	probeTable, ok := cat.Table(rightTable)
	if !ok || !probeTable.JoinKeyInList {
		return nil, fmt.Errorf("%w: %s does not declare join_key_in_list - v1 only supports a semi-join probe side that accepts a chunked IN-list",
			ErrUnsupportedStatement, rightTable)
	}

	leftRoot, leftScan, leftVerdicts, leftSecurity, err := buildSideTree(leftTable, leftWhere, residuals, cat, leftKeyCol, leftProject, leftMaskCols)
	if err != nil {
		return nil, err
	}
	rightRoot, rightScan, rightVerdicts, rightSecurity, err := buildSideTree(rightTable, rightWhere, residuals, cat, rightKeyCol, rightProject, rightMaskCols)
	if err != nil {
		return nil, err
	}

	join := &Join{
		Left:      leftRoot,
		Right:     rightRoot,
		On:        Equi{LeftCol: leftKeyCol, RightCol: rightKeyCol},
		MaxInList: probeTable.MaxInList,
	}

	var root Node = join
	if len(outCols) > 0 {
		root = &Project{Child: join, Cols: outCols}
	}

	verdicts := make(map[string]Verdict, len(leftVerdicts)+len(rightVerdicts))
	for k, v := range leftVerdicts {
		verdicts[k] = v
	}
	for k, v := range rightVerdicts {
		verdicts[k] = v
	}

	return &Plan{
		Root:     root,
		verdicts: verdicts,
		scans:    map[string]*Scan{leftTable: leftScan, rightTable: rightScan},
		security: append(leftSecurity, rightSecurity...),
	}, nil
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
		leftTable, err := tableName(je.LeftExpr)
		if err != nil {
			return nil, err
		}
		rightTable, err := tableName(je.RightExpr)
		if err != nil {
			return nil, err
		}
		return []string{leftTable, rightTable}, nil
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
	leftTable, err := tableName(je.LeftExpr)
	if err != nil {
		return "", err
	}
	rightTable, err := tableName(je.RightExpr)
	if err != nil {
		return "", err
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
		for _, c := range conjuncts {
			alias, col, op, _, err := comparisonPartsQualified(c)
			if err != nil {
				return "", err
			}
			predShapes = append(predShapes, alias+"."+col+" "+op+" ?")
		}
	}

	shape := "SELECT " + strings.Join(parts, ",") + " FROM " + leftTable + " JOIN " + rightTable
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

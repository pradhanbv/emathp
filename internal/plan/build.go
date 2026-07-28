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

// PrimaryScan returns the plan's one Scan. v1 is single-table only, so
// there's never more than one to choose between; joins (Cycle 10) will
// need callers to go through Scan(table) instead.
func (p *Plan) PrimaryScan() *Scan {
	for _, s := range p.scans {
		return s
	}
	return nil
}

// Filters returns every Filter node in the tree, pushed and local alike -
// what exec needs to know both what to send to the connector and what to
// re-check afterward.
func (p *Plan) Filters() []*Filter {
	var out []*Filter
	walk(p.Root, func(n Node) {
		if f, ok := n.(*Filter); ok {
			out = append(out, f)
		}
	})
	return out
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

// Build parses sql (v1 surface: single table, conjunctive WHERE, simple
// column projection) and classifies every predicate - from the query text
// and from residuals - against cat's capability profile for the table.
// masks declares which output columns get CLS masking applied.
func Build(sql string, cat *catalog.Catalog, residuals policy.Residuals, masks policy.Masks) (*Plan, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("plan: parse: %w", err)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok || len(sel.From) != 1 {
		return nil, ErrUnsupportedStatement
	}

	table, err := tableName(sel.From[0])
	if err != nil {
		return nil, err
	}

	projectCols, err := projection(sel.SelectExprs)
	if err != nil {
		return nil, err
	}

	scan := &Scan{Table: table}
	verdicts := make(map[string]Verdict)
	var root Node = scan

	// predicateCols collects every predicate's column, pushed or local,
	// in first-seen order. A PUSHED_ENFORCED predicate's column still
	// needs fetching: the verification filter (Cycle 6) re-checks it
	// locally after fetch, so over-projection applies to it too.
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
			applyPredicate(col, op, value, UserOrigin)
		}
	}

	var security []securityPredicate
	for _, r := range residuals {
		if r.Table != table {
			continue
		}
		col, op, value, err := residualParts(r.Expr)
		if err != nil {
			return nil, err
		}
		v := applyPredicate(col, op, value, SecurityOrigin)
		security = append(security, securityPredicate{
			Pred:    Predicate{Column: col, Op: op, Value: value, Origin: SecurityOrigin},
			Verdict: v,
		})
	}

	var maskCols []string
	if len(projectCols) > 0 {
		cols := make([]ProjectCol, len(projectCols))
		for i, c := range projectCols {
			cols[i] = ProjectCol{Name: c}
			for _, m := range masks {
				if m.Table == table && m.Column == c {
					fn := m.Fn
					cols[i].Mask = &fn
					maskCols = append(maskCols, c)
				}
			}
		}
		root = &Project{Child: root, Cols: cols}
	}

	// A mask is about to be applied locally - assert the catalog agrees
	// this connector can't mask itself. v1 has no pushed-masking
	// implementation, so a catalog claiming otherwise is a configuration
	// error to catch now, not a capability to silently ignore.
	if len(maskCols) > 0 {
		t, ok := cat.Table(table)
		if !ok || t.Masking != catalog.MaskingUnsupported {
			return nil, fmt.Errorf("%w: %s declares masking %q", ErrMaskingUnsupported, table, t.Masking)
		}
	}

	// The scan projects the union of the output columns, every predicate
	// column (pushed or local), and mask columns - then the top Project
	// trims back. Join keys join this union in Cycle 10.
	scan.Project = dedupUnion(projectCols, predicateCols, maskCols)

	return &Plan{
		Root:     root,
		verdicts: verdicts,
		scans:    map[string]*Scan{table: scan},
		security: security,
	}, nil
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

// ParseTable extracts the table sql queries, without building a plan.
// Orchestration code needs the table before it can ask a policy.Provider
// for residuals, which Build then takes as an argument.
func ParseTable(sql string) (string, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("plan: parse: %w", err)
	}
	sel, ok := stmt.(*sqlparser.Select)
	if !ok || len(sel.From) != 1 {
		return "", ErrUnsupportedStatement
	}
	return tableName(sel.From[0])
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

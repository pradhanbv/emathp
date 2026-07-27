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
}

func (p *Plan) VerdictFor(expr string) Verdict {
	return p.verdicts[normalizeExpr(expr)]
}

func (p *Plan) Scan(table string) *Scan {
	return p.scans[table]
}

func normalizeExpr(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

var parser = sqlparser.NewTestParser()

// Build parses sql (v1 surface: single table, conjunctive WHERE, simple
// column projection) and classifies every predicate - from the query text
// and from residuals - against cat's capability profile for the table.
func Build(sql string, cat *catalog.Catalog, residuals policy.Residuals) (*Plan, error) {
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

	scan := &Scan{Table: table, Project: projectCols}
	verdicts := make(map[string]Verdict)
	var root Node = scan

	applyPredicate := func(col, op, value string, origin Origin) {
		exprText := normalizeExpr(col + " " + op + " " + value)
		v := classify(cat, table, col, op)
		verdicts[exprText] = v
		if v.Site == Pushed {
			scan.Pushed = append(scan.Pushed, col)
		}
		root = &Filter{
			Child: root,
			Pred:  Predicate{Column: col, Op: op, Value: value, Origin: origin},
			Site:  v.Site,
		}
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

	for _, r := range residuals {
		if r.Table != table {
			continue
		}
		col, op, value, err := residualParts(r.Expr)
		if err != nil {
			return nil, err
		}
		applyPredicate(col, op, value, SecurityOrigin)
	}

	if len(projectCols) > 0 {
		cols := make([]ProjectCol, len(projectCols))
		for i, c := range projectCols {
			cols[i] = ProjectCol{Name: c}
		}
		root = &Project{Child: root, Cols: cols}
	}

	return &Plan{
		Root:     root,
		verdicts: verdicts,
		scans:    map[string]*Scan{table: scan},
	}, nil
}

// classify is the capability walk: ENFORCED -> push, ADVISORY -> retain
// locally, unknown column/op -> retain locally as Unsupported.
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

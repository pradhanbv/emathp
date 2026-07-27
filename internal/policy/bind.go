package policy

import (
	"fmt"
	"strings"
)

// BindResiduals substitutes $principal.<attr> placeholders in each
// residual's expression with the resolved principal's attrs.
//
// ADR-003 has real parameter binding happen after a plan-cache lookup, on
// an already-cached parameterized plan. This MVP has no plan cache yet
// (that's Cycle 7), so there's nothing to parameterize against - binding
// here, before Build, is a deliberate simplification, not the final
// architecture. It's revisited when the plan cache lands.
func BindResiduals(residuals Residuals, attrs map[string]string) (Residuals, error) {
	out := make(Residuals, len(residuals))
	for i, r := range residuals {
		expr, err := bindExpr(r.Expr, attrs)
		if err != nil {
			return nil, err
		}
		out[i] = Residual{Table: r.Table, Expr: expr}
	}
	return out, nil
}

func bindExpr(expr string, attrs map[string]string) (string, error) {
	parts := strings.Fields(expr)
	if len(parts) != 3 {
		return "", fmt.Errorf("policy: residual expr must be 'column op value', got %q", expr)
	}

	const prefix = "$principal."
	if strings.HasPrefix(parts[2], prefix) {
		attr := strings.TrimPrefix(parts[2], prefix)
		v, ok := attrs[attr]
		if !ok {
			return "", fmt.Errorf("policy: missing principal attribute %q", attr)
		}
		parts[2] = "'" + v + "'"
	}
	return strings.Join(parts, " "), nil
}

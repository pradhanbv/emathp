// Package policy holds tenant policy compiled toward the plan: row-level
// security residuals that get injected as Filter nodes rather than applied
// as a post-filter (ADR-002). This cycle defines the shape the planner
// consumes; the provider that produces it from OPA-style rules lands with
// RLS injection.
package policy

// Residual is a security predicate that must hold for a query against
// Table, expressed as raw "<column> <op> <value>" text (value may be a
// $principal.<attr> placeholder, resolved before execution).
type Residual struct {
	Table string
	Expr  string
}

type Residuals []Residual

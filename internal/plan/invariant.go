package plan

import (
	"errors"
	"fmt"
)

// ErrEntitlementDenied maps to ENTITLEMENT_DENIED: a security predicate
// went unaccounted for in the plan. This is ADR-002's fail-closed
// invariant, asserted after parameter binding, including on plan-cache
// hits.
var ErrEntitlementDenied = errors.New("entitlement denied")

// FindFilter returns the first Filter node in p's tree predicating on
// column, or nil if none does.
func FindFilter(p *Plan, column string) *Filter {
	var found *Filter
	walk(p.Root, func(n Node) {
		if found != nil {
			return
		}
		if f, ok := n.(*Filter); ok && f.Pred.Column == column {
			found = f
		}
	})
	return found
}

// RemoveFilter splices the Filter node for column out of p's tree.
// Test-only fault injection standing in for a rule-based optimizer rewrite
// this planner doesn't implement (ADR-001) - Build itself is
// invariant-preserving by construction, so this is the only way to reach
// the state AssertInvariant needs to fail closed against. See
// IMPLEMENTATION_PLAN.md Cycle 3.
func RemoveFilter(p *Plan, column string) {
	p.Root = removeFilter(p.Root, column)
}

func removeFilter(n Node, column string) Node {
	switch t := n.(type) {
	case *Filter:
		t.Child = removeFilter(t.Child, column)
		if t.Pred.Column == column {
			return t.Child
		}
		return t
	case *Project:
		t.Child = removeFilter(t.Child, column)
		return t
	case *Join:
		for i := range t.Sides {
			t.Sides[i].Root = removeFilter(t.Sides[i].Root, column)
		}
		return t
	default:
		return n
	}
}

// AssertInvariant is the ADR-002 plan-time check: every security
// predicate Build injected must still be present in the tree as either a
// PUSHED_ENFORCED filter or a locally retained residual filter - never
// silently dropped. It does not catch a connector that lies about
// enforcing what it was pushed; that is the runtime verification filter's
// job, a different mechanism entirely.
func AssertInvariant(p *Plan) error {
	present := make(map[string]*Filter)
	walk(p.Root, func(n Node) {
		if f, ok := n.(*Filter); ok && f.Pred.Origin == SecurityOrigin {
			present[predicateKey(f.Pred)] = f
		}
	})

	for _, sp := range p.security {
		f, ok := present[predicateKey(sp.Pred)]
		if !ok {
			return fmt.Errorf("%w: security predicate %s %s %s missing from plan",
				ErrEntitlementDenied, sp.Pred.Column, sp.Pred.Op, sp.Pred.Value)
		}

		switch sp.Verdict.Disposition {
		case PushedEnforced:
			if f.Site != Pushed {
				return fmt.Errorf("%w: security predicate %s must be pushed to an ENFORCED site",
					ErrEntitlementDenied, sp.Pred.Column)
			}
		default: // Residual, Unsupported: never dropped, only retained locally
			if f.Site != Local {
				return fmt.Errorf("%w: security predicate %s must be retained as a local filter",
					ErrEntitlementDenied, sp.Pred.Column)
			}
		}
	}
	return nil
}

func predicateKey(p Predicate) string {
	return p.Column + "|" + p.Op + "|" + p.Value
}

// walk visits every node in the tree rooted at n, depth-first.
func walk(n Node, visit func(Node)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Children() {
		walk(c, visit)
	}
}

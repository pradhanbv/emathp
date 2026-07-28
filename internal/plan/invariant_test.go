package plan_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/policy"
)

// personaRole maps the short persona names used in tests (mirroring
// harness.Token) to the policy role they resolve to via identity - e.g.
// dana's token groups resolve to "support_agent".
var personaRole = map[string]string{
	"support": "support_agent",
}

// buildFor is the orchestration a request handler will eventually do:
// resolve the table, ask policy for that role's residuals against it,
// build the plan.
func buildFor(t *testing.T, persona, sql string) *plan.Plan {
	t.Helper()

	cat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)

	pol, err := policy.Load("../../testdata/policy")
	require.NoError(t, err)

	tables, err := plan.ParseTables(sql)
	require.NoError(t, err)
	table := tables[0]

	residuals, err := pol.ResidualsFor(personaRole[persona], table)
	require.NoError(t, err)

	p, err := plan.Build(sql, cat, residuals, nil)
	require.NoError(t, err)
	return p
}

// TestRLSInjectedAsFilter proves policy compiles into the plan as a real
// Filter node - never as a post-filter bolted on after execution.
func TestRLSInjectedAsFilter(t *testing.T) {
	p := buildFor(t, "support", "SELECT id FROM sf.accounts")

	f := plan.FindFilter(p, "region")
	require.NotNil(t, f, "RLS must appear as a Filter node")
	require.Equal(t, plan.SecurityOrigin, f.Pred.Origin)
	require.Equal(t, plan.Local, f.Site) // region is ADVISORY - retained, not pushed
}

// TestInvariantFailsClosedOnDroppedPredicate is the ADR-002 plan-time
// check: if something strips a security predicate's Filter node from the
// tree (e.g. an optimizer bug), the plan must not be allowed to execute.
//
// RemoveFilter is fault injection, not a shortcut: Build classifies and
// injects each predicate atomically, so no catalog/policy fixture can make
// it emit a plan that's already missing one - the bad state this test
// needs isn't reachable through Build's own inputs. RemoveFilter simulates
// the rule-based optimizer rewrite (ADR-001) that would produce it in the
// real system but doesn't exist in this planner yet. See
// IMPLEMENTATION_PLAN.md Cycle 3 for the full rationale, including why
// this is also what forces AssertInvariant to check the tree against a
// pre-mutation snapshot (Plan.security) rather than against itself.
func TestInvariantFailsClosedOnDroppedPredicate(t *testing.T) {
	p := buildFor(t, "support", "SELECT id FROM sf.accounts")

	plan.RemoveFilter(p, "region")

	require.ErrorIs(t, plan.AssertInvariant(p), plan.ErrEntitlementDenied)
}

// TestInvariantPassesWhenPolicyIsIntact is the negative pairing: the
// invariant must not fire on an untouched plan, or it isn't testing
// anything.
func TestInvariantPassesWhenPolicyIsIntact(t *testing.T) {
	p := buildFor(t, "support", "SELECT id FROM sf.accounts")

	require.NoError(t, plan.AssertInvariant(p))
}

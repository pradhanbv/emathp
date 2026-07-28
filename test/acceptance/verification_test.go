package acceptance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// enforcedRegionCatalogDir is a temp catalog where region is ENFORCED,
// not the shared fixture's ADVISORY - needed so our planner actually
// pushes the RLS predicate (only a PUSHED security predicate goes through
// the verification filter at all; an ADVISORY one is already local and
// has nothing to verify).
func enforcedRegionCatalogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fixture := `{
		"table": "sf.accounts",
		"predicates": {
			"status": { "ops": ["="], "enforcement": "ENFORCED" },
			"region": { "ops": ["="], "enforcement": "ENFORCED" }
		},
		"masking": "unsupported"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sf.accounts.json"), []byte(fixture), 0o644))
	return dir
}

// TestHonestConnectorZeroViolations is the negative pairing the headline
// test needs - proving the verification filter isn't trivially
// always-firing. An honest connector that actually applies the ENFORCED
// filter it declares must produce zero violations.
func TestHonestConnectorZeroViolations(t *testing.T) {
	obs.EnforcedPredicateViolations.Reset()

	sf := mocksf.Start(t, mocksf.Rows(10), mocksf.Capability("region", catalog.Enforced))
	gw := harness.Start(t, testDepsWithCatalog(t, sf, enforcedRegionCatalogDir(t)))

	res := gw.POST("/v1/query", `{"sql":"SELECT id,region FROM sf.accounts"}`, harness.Token("support"))

	require.Equal(t, 200, res.Code)
	require.Zero(t, obs.EnforcedPredicateViolations.Value())
}

// TestLyingConnectorFailsClosed is the headline test the whole submission
// carries. The plan-time invariant (Cycle 3) passes here - region *was*
// legitimately pushed to a connector that claimed to enforce it. Only the
// runtime verification filter, re-applying the predicate locally after
// fetch, notices the connector's real behaviour diverged from its
// declaration.
func TestLyingConnectorFailsClosed(t *testing.T) {
	obs.EnforcedPredicateViolations.Reset()

	sf := mocksf.Start(t, mocksf.Rows(10), mocksf.Capability("region", catalog.Enforced), mocksf.LieAbout("region"))
	gw := harness.Start(t, testDepsWithCatalog(t, sf, enforcedRegionCatalogDir(t)))

	res := gw.POST("/v1/query", `{"sql":"SELECT id,region FROM sf.accounts"}`, harness.Token("support"))

	require.Equal(t, 403, res.Code)
	require.Equal(t, "ENTITLEMENT_DENIED", res.Body.Error.Code)
	require.Empty(t, res.Body.Rows, "must not serve rows from a connector that lied")
	require.Positive(t, obs.EnforcedPredicateViolations.Value())
}

// TestObjectLevelAuthzDeniesOutOfScopeTable proves Layer 1 (DESIGN.md
// ADR-002): support_agent's policy denies sf.opportunities outright, at
// admission, before any catalog lookup or planning happens. This test had
// no home anywhere in the original cycle plan despite DESIGN.md claiming
// all three layers are proven end-to-end - see IMPLEMENTATION_PLAN.md
// Cycle 6.
func TestObjectLevelAuthzDeniesOutOfScopeTable(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	res := gw.POST("/v1/query", `{"sql":"SELECT id FROM sf.opportunities"}`, harness.Token("support"))

	require.Equal(t, 403, res.Code)
	require.Equal(t, "ENTITLEMENT_DENIED", res.Body.Error.Code)
}

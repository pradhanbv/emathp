package plan_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/policy"
)

// TestCapabilityClassification proves the same predicate classifies
// differently depending on the table's declared capability profile:
// status is ENFORCED (push it), region is ADVISORY (retain it locally).
func TestCapabilityClassification(t *testing.T) {
	cat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)

	p, err := plan.Build("SELECT id,region FROM sf.accounts WHERE status='open'", cat,
		policy.Residuals{{Table: "sf.accounts", Expr: "region = $principal.region"}}, nil)
	require.NoError(t, err)

	require.Equal(t, plan.PushedEnforced, p.VerdictFor("status = 'open'").Disposition)
	require.Equal(t, plan.Residual, p.VerdictFor("region = $principal.region").Disposition)
	require.Contains(t, p.Scan("sf.accounts").Pushed, "status")
	require.NotContains(t, p.Scan("sf.accounts").Pushed, "region")
}

// TestSamePredicateDifferentCapabilityProfile is the cycle's literal "done
// when": the exact same predicate classifies differently depending on
// which connector's capability profile it's checked against.
func TestSamePredicateDifferentCapabilityProfile(t *testing.T) {
	dir := t.TempDir()
	fixture := `{
		"table": "sf.accounts",
		"predicates": { "status": { "ops": ["="], "enforcement": "ADVISORY" } }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sf.accounts.json"), []byte(fixture), 0o644))

	advisoryCat, err := catalog.Load(dir)
	require.NoError(t, err)
	enforcedCat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)

	const sql = "SELECT id FROM sf.accounts WHERE status='open'"

	advisoryPlan, err := plan.Build(sql, advisoryCat, nil, nil)
	require.NoError(t, err)
	enforcedPlan, err := plan.Build(sql, enforcedCat, nil, nil)
	require.NoError(t, err)

	require.Equal(t, plan.Residual, advisoryPlan.VerdictFor("status = 'open'").Disposition)
	require.Equal(t, plan.PushedEnforced, enforcedPlan.VerdictFor("status = 'open'").Disposition)
}

// TestMaskingCapabilityValidated: v1 has no pushed-masking implementation,
// so a catalog claiming a connector CAN mask itself is a configuration
// error to catch at build time - see DESIGN.md ADR-002 on why source-side
// masking is the exception for the SaaS-REST-API connectors this design
// targets, not something worth branching on.
func TestMaskingCapabilityValidated(t *testing.T) {
	dir := t.TempDir()
	fixture := `{
		"table": "sf.accounts",
		"predicates": { "status": { "ops": ["="], "enforcement": "ENFORCED" } },
		"masking": "supported"
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sf.accounts.json"), []byte(fixture), 0o644))
	cat, err := catalog.Load(dir)
	require.NoError(t, err)

	_, err = plan.Build("SELECT email FROM sf.accounts", cat, nil,
		policy.Masks{{Table: "sf.accounts", Column: "email", Fn: "sha256"}})

	require.ErrorIs(t, err, plan.ErrMaskingUnsupported)
}

package plancache_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/exec"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/plancache"
	"github.com/pradhanbv/emathp/internal/policy"
)

// personaRole mirrors the convention every other cycle's tests use: short
// persona names mapped to the policy role they resolve to.
var personaRole = map[string]string{
	"support": "support_agent",
	"admin":   "admin",
}

type planResult struct {
	Plan     *plan.Plan
	CacheKey plancache.Key
	CacheHit bool
	Masks    []string
}

// sharedCache is reset at the start of every test - a Cache is meant to
// live for a gateway process's whole lifetime, but each test needs one
// starting empty, the same reason obs.EnforcedPredicateViolations needed
// Reset() in Cycle 6.
var sharedCache = plancache.New()

func planFor(t *testing.T, persona, sql string) planResult {
	t.Helper()

	cat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)

	pol, err := policy.Load("../../testdata/policy")
	require.NoError(t, err)

	p, key, hit, err := plancache.Resolve(sharedCache, sql, cat, pol, "t_acme", personaRole[persona])
	require.NoError(t, err)

	var masks []string
	for _, c := range p.OutputColumns() {
		if c.Mask != nil {
			masks = append(masks, c.Name)
		}
	}

	return planResult{Plan: p, CacheKey: key, CacheHit: hit, Masks: masks}
}

// capturingSource is a minimal connector.Source recording the last
// Filters it was asked to fetch with - just enough to prove which literal
// value actually reached the connector.
type capturingSource struct {
	lastFilters map[string][]string
}

func (s *capturingSource) Fetch(_ context.Context, req connector.FetchRequest) ([]connector.Row, connector.FetchMeta, error) {
	s.lastFilters = req.Filters
	return nil, connector.FetchMeta{}, nil
}

// TestPlanCacheDoesNotLeakAcrossRoles is the privilege-escalation test:
// admin and support_agent must never share a cache entry, even for the
// identical SQL text, because the key includes role - and the plans
// really do differ (support gets email masked, admin doesn't).
func TestPlanCacheDoesNotLeakAcrossRoles(t *testing.T) {
	sharedCache.Reset()

	const sql = "SELECT id,email FROM sf.accounts"
	admin := planFor(t, "admin", sql)
	support := planFor(t, "support", sql)

	require.NotEqual(t, admin.CacheKey, support.CacheKey)
	require.False(t, support.CacheHit, "support must not receive admin's plan")
	require.Contains(t, support.Masks, "email")
	require.NotContains(t, admin.Masks, "email")
}

// TestPlanCacheHitsOnSameShapeDifferentValue is the other half: the cache
// has to be loose enough to be worth having. Two queries differing only
// in a WHERE-clause literal must share a plan - and, just as importantly,
// each must still see its OWN literal at the connector. That second half
// didn't exist here until Cycle 13: for six cycles this test only checked
// CacheKey equality and CacheHit, never that the second query's own value
// reached anything - which is exactly the shape of the bug that let a
// stale-bound literal (Build baking the WHERE-clause value into the
// cached Plan itself) go undetected. Fixed by resolving literals lazily,
// the same way $principal.<attr> already was - see plan.ExtractParams and
// exec.Run's params argument.
func TestPlanCacheHitsOnSameShapeDifferentValue(t *testing.T) {
	sharedCache.Reset()

	const sqlA = "SELECT id FROM sf.accounts WHERE status='open'"
	const sqlB = "SELECT id FROM sf.accounts WHERE status='closed'"

	a := planFor(t, "support", sqlA)
	b := planFor(t, "support", sqlB)

	require.Equal(t, a.CacheKey, b.CacheKey) // parameterized
	require.True(t, b.CacheHit)

	source := &capturingSource{}
	sources := map[string]connector.Source{"sf": source}

	paramsA, err := plan.ExtractParams(sqlA)
	require.NoError(t, err)
	_, err = exec.Run(context.Background(), a.Plan, sources, nil, paramsA)
	require.NoError(t, err)
	require.Equal(t, []string{"open"}, source.lastFilters["status"])

	paramsB, err := plan.ExtractParams(sqlB)
	require.NoError(t, err)
	_, err = exec.Run(context.Background(), b.Plan, sources, nil, paramsB)
	require.NoError(t, err)
	require.Equal(t, []string{"closed"}, source.lastFilters["status"],
		"a cache hit must not reuse the first query's literal value")
}

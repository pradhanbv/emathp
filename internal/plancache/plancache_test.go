package plancache_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
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

	return planResult{CacheKey: key, CacheHit: hit, Masks: masks}
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
// in a WHERE-clause literal must share a plan.
func TestPlanCacheHitsOnSameShapeDifferentValue(t *testing.T) {
	sharedCache.Reset()

	a := planFor(t, "support", "SELECT id FROM sf.accounts WHERE status='open'")
	b := planFor(t, "support", "SELECT id FROM sf.accounts WHERE status='closed'")

	require.Equal(t, a.CacheKey, b.CacheKey) // parameterized
	require.True(t, b.CacheHit)
}

package acceptance

import (
	"time"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestMaxStalenessServesCache proves the cache-hit half of ADR-005: a
// second query within the same max_staleness budget is served from memory
// - zero extra calls to the connector - and reports itself as a cache hit
// with a freshness age under the budget.
func TestMaxStalenessServesCache(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	gw.QueryFresh("support", simpleSQL, "60s")
	before := sf.CallCount()
	res := gw.QueryFresh("support", simpleSQL, "60s")

	require.Equal(t, 200, res.Code)
	require.Equal(t, before, sf.CallCount(), "within TTL => no live fetch")
	require.NotNil(t, res.Body.Meta)
	require.True(t, res.Body.Meta.CacheHit)
	require.NotNil(t, res.Body.FreshnessMS)
	require.Less(t, *res.Body.FreshnessMS, int64(60_000))
}

// TestETagRevalidationSpendsBudget proves the half that's easy to get
// wrong: once the cache entry goes stale, revalidating it (a conditional
// fetch that comes back 304, unchanged) still spends a rate-limit token.
// Treating a "nothing changed" response as free would silently exceed a
// real source's quota - the doc calls this out directly (Section 5, ADR-005).
func TestETagRevalidationSpendsBudget(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	deps := testDeps(t, sf)
	deps.RateLimit.SetLimit("sf", 10)
	gw := harness.Start(t, deps)

	gw.QueryFresh("support", simpleSQL, "60s")

	staleAt := time.Now().Add(90 * time.Second)
	deps.Freshness.Now = func() time.Time { return staleAt }

	before := deps.RateLimit.Remaining("sf")
	res := gw.QueryFresh("support", simpleSQL, "60s")

	require.Equal(t, 200, res.Code)
	require.NotNil(t, res.Body.Meta)
	require.True(t, res.Body.Meta.Revalidated)
	require.Less(t, deps.RateLimit.Remaining("sf"), before,
		"ADR-005: a freshness probe spends a token")
}

// TestFreshnessCacheIsolatedByPrincipal proves the result-cache key
// addendum in DESIGN.md ADR-002: two different principals issuing the
// identical query must never share one cache entry, even when neither our
// own RLS nor the mock connector gives the key any other reason to differ.
// "support" and "support2" are deliberately the SAME role and region
// (dana and erin both resolve to support_agent/EMEA via group 8f3c-4d21,
// see fixtures.IssuerRegistry) - only their principal identity (sub)
// differs, so the RLS-driven required-column set and pushed filters are
// byte-identical between them. That isolates the property this cycle
// actually fixes: two DIFFERENT users, SAME role, must still not share a
// cache entry, because layer 3 (source-side sharing) answers to the
// individual, not the role (ADR-002). Using "support" vs "admin" instead
// would conflate this with role-driven policy differences (support_agent's
// RLS pulls region into required columns; admin has none), which the old
// key already differentiated on Columns alone - a weaker, misleading test.
// This can't be proven by comparing row content either - mocksf returns
// the same rows to every caller regardless of principal, since it doesn't
// model layer-3 (source-side, per-user) sharing rules at all - so it
// proves the mechanism instead: a connector call count that must be 2.
func TestFreshnessCacheIsolatedByPrincipal(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	gw.QueryFresh("support", simpleSQL, "60s")  // dana - u_8f31c2
	gw.QueryFresh("support2", simpleSQL, "60s") // erin - u_er1n02, same role+region

	require.Equal(t, 2, sf.CallCount(),
		"two principals, identical query, must not share one cache entry")
}

// TestResultCacheHitRatioMetric proves result_cache_requests_total records
// exactly one miss (the first, uncached call) and one hit (the second,
// within max_staleness) for a single principal - the counter DESIGN.md
// Section 9 says result_cache_hit_ratio is derived from via PromQL rate(),
// rather than something the gateway computes and stores itself.
func TestResultCacheHitRatioMetric(t *testing.T) {
	obs.ResetMetrics()
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	gw.QueryFresh("support", simpleSQL, "60s") // miss
	gw.QueryFresh("support", simpleSQL, "60s") // hit

	require.Equal(t, 1, obs.Gather("result_cache_requests_total",
		map[string]string{"connector": "sf", "outcome": "hit"}).SampleCount)
	require.Equal(t, 1, obs.Gather("result_cache_requests_total",
		map[string]string{"connector": "sf", "outcome": "miss"}).SampleCount)
}

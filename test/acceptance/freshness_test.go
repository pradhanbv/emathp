package acceptance

import (
	"time"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
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

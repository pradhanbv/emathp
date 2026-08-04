package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestPaginatedFetchSpendsOneTokenPerPage pins the unit the rate-limit
// budget is denominated in. A vendor bills per HTTP request, and
// HTTPSource paginates inside a single Fetch - so a 1,200-row result at a
// 500-row page size is three billable requests, not one.
//
// The budget used to be spent once per logical Fetch, up in the freshness
// layer above the pagination loop, which under-counted by the pagination
// factor. Nothing caught it because every rate-limit test used
// mocksf.Rows(5): one page, where one token and one request happen to be
// the same number. The mocks were right all along - callCount.Add(1) sits
// in the HTTP handler, and mocksf.RateLimit checks against that - so the
// mock modelled the vendor per-request while the gateway did not.
func TestPaginatedFetchSpendsOneTokenPerPage(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(1200), mocksf.PageSize(500))
	deps := testDeps(t, sf)
	deps.RateLimit.SetLimit("sf", 100)
	gw := harness.Start(t, deps)

	before := deps.RateLimit.Remaining("sf")
	res := gw.Query("support", simpleSQL)
	require.Equal(t, 200, res.Code)

	spent := before - deps.RateLimit.Remaining("sf")
	require.Equal(t, 3, spent,
		"1,200 rows at a 500-row page size is 3 HTTP requests, so 3 tokens")
	require.Equal(t, 3, sf.CallCount(),
		"and the source agrees on the count - tokens must track real requests")
}

// TestBudgetExhaustedMidPaginationFailsTheQuery fixes the behaviour at the
// boundary the per-page gate creates: a fetch can now run out of budget
// after collecting some pages. It fails the query rather than returning
// the rows it happened to get. A SQL result that silently omits rows is
// worse than an error, and ADR-009's partial-result path is explicit and
// opt-in - this is not that path. The tokens already spent stay spent;
// they bought real API calls.
func TestBudgetExhaustedMidPaginationFailsTheQuery(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(1200), mocksf.PageSize(500))
	deps := testDeps(t, sf)
	deps.RateLimit.SetLimit("sf", 2) // needs 3 pages, has budget for 2
	gw := harness.Start(t, deps)

	res := gw.Query("support", simpleSQL)

	require.Equal(t, 429, res.Code)
	require.Equal(t, "RATE_LIMIT_EXHAUSTED", res.Body.Error.Code)
	require.Empty(t, res.Body.Rows, "must not serve a partial page set as if complete")
	require.Equal(t, 2, sf.CallCount(), "stopped at the budget, did not fetch page 3")
}

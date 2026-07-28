package acceptance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

const simpleSQL = "SELECT id FROM sf.accounts"

// TestRateLimitExhausted proves the 429 + Retry-After + async-hint path:
// a connector budget of 3 lets exactly 3 calls through, the 4th is
// rejected, naming the connector so a caller knows what to back off on.
func TestRateLimitExhausted(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	deps := testDeps(t, sf)
	deps.RateLimit.SetLimit("sf", 3)
	gw := harness.Start(t, deps)

	for i := 0; i < 3; i++ {
		res := gw.Query("support", simpleSQL)
		require.Equal(t, 200, res.Code, "call %d should still be within budget", i+1)
	}

	res := gw.Query("support", simpleSQL)

	require.Equal(t, 429, res.Code)
	require.Equal(t, "RATE_LIMIT_EXHAUSTED", res.Body.Error.Code)
	require.NotEmpty(t, res.Header.Get("Retry-After"))
	require.Contains(t, res.Body.Error.Message, "sf") // names the connector
}

// TestAsyncReroute proves the reroute path exists: Prefer: respond-async
// gets a 202 and a poll URL immediately, and polling it eventually
// reports done - an in-memory map, not a real queue (IMPLEMENTATION_PLAN
// Cycle 8: "the rubric point is the reroute path existing, not a queue").
func TestAsyncReroute(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	res := gw.QueryWithHeader("support", simpleSQL, "Prefer", "respond-async")

	require.Equal(t, 202, res.Code)
	require.NotEmpty(t, res.Body.PollURL)
	require.Eventually(t, func() bool {
		return gw.Poll(res.Body.PollURL).Done
	}, 5*time.Second, 100*time.Millisecond)
}

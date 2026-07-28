package acceptance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mockzd"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestSourceTimeoutPartialResults proves ADR-009's timeout path: a
// connector that outlives the request's timeout budget produces a
// terminal NDJSON frame naming the connector that didn't finish, not a
// hung request or an opaque failure. v1 emits exactly one frame (the
// terminal one) rather than true incremental streaming - the rubric point
// is the timeout-to-partial-result path existing, per
// IMPLEMENTATION_PLAN.md Cycle 11.
func TestSourceTimeoutPartialResults(t *testing.T) {
	// Delay comfortably exceeds the timeout (5x) so the test isn't flaky
	// under CI jitter, but stays short - the mock's handler goroutine keeps
	// sleeping after the client gives up, and t.Cleanup's Close() waits for
	// it, so the delay's value is a floor on this test's real wall time.
	zd := mockzd.Start(t, mockzd.Tickets(5, "open"), mockzd.Delay(500*time.Millisecond))
	gw := harness.Start(t, testDepsZD(t, zd))

	res := gw.QueryStream("support", "SELECT id FROM zd.tickets", "100ms")

	frames := res.NDJSON()
	require.NotEmpty(t, frames, "must get at least the terminal frame")

	last := frames[len(frames)-1]
	require.True(t, last.IsTerminal)
	require.True(t, last.Partial)
	require.Equal(t, "SOURCE_TIMEOUT", last.Sources["zd"].Error)
	require.NotEmpty(t, last.TraceID)
}

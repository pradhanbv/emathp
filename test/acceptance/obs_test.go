package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestConnectorDurationMetric proves a connector fetch is observable: the
// gateway records connector_request_duration_seconds labeled by connector
// name, not just logs a line nobody queries.
func TestConnectorDurationMetric(t *testing.T) {
	obs.ResetMetrics()
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	gw.Query("support", simpleSQL)

	require.Positive(t, obs.Gather("connector_request_duration_seconds",
		map[string]string{"connector": "sf"}).SampleCount)
}

// TestTraceIDPropagates proves the trace id the gateway hands back to its
// caller is the same one it sent on to the connector - a caller can hand
// that id to support and it'll actually be findable in the connector's own
// logs, not just decoration on the gateway's own response.
func TestTraceIDPropagates(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	res := gw.Query("support", simpleSQL)

	require.NotEmpty(t, res.Body.TraceID)
	require.Equal(t, res.Body.TraceID, sf.LastRequest().Header.Get("X-Trace-Id"))
}

package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

func TestQueryEnvelope(t *testing.T) {
	gw := harness.Start(t)
	res := gw.POST("/v1/query", `{"sql":"SELECT id FROM sf.accounts"}`,
		harness.Token("support"))

	require.Equal(t, 200, res.Code)
	require.NotEmpty(t, res.Body.Columns)
	require.NotEmpty(t, res.Body.TraceID)
	require.NotNil(t, res.Body.FreshnessMS)
	require.Contains(t, res.Body.RateLimitStatus, "sf")
}

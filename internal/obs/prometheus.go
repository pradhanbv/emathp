package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ConnectorRequestDuration is the real, scrapeable backing for
// connector_request_duration_seconds - registered against the default
// Prometheus registry, which GET /metrics (internal/server) exposes in
// standard exposition format. Distinct from the in-memory Observe/Gather
// pair above: those exist for tests to assert "a sample was recorded"
// without a real Prometheus registry in the loop; this is the metric an
// actual Prometheus server scrapes and graphs. Both are fed from the same
// call site (internal/freshness's timedFetch) so neither can drift from
// what actually happened on a connector call.
var ConnectorRequestDuration = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "connector_request_duration_seconds",
		Help:    "Duration of a connector fetch (one exec.Run-level call, pagination included), by connector and outcome.",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"connector", "outcome"},
)

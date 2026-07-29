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

// ResultCacheRequests is the raw signal result_cache_hit_ratio derives from
// (DESIGN.md Section 9): outcome="hit" means internal/freshness served
// stored rows with no outbound call; outcome="miss" means a live or
// conditional fetch was made, whether or not it came back 304. The ratio
// itself is a PromQL rate() over this counter, not a value tracked
// in-process - that lets a dashboard, an alert, and the Section 5.3
// principal-count measurement each pick their own window over one signal.
// No principal label here even though the cache key it mirrors does carry
// one (freshness.go's cacheKey) - a per-principal label on a Prometheus
// series is a cardinality explosion at 10M users the in-memory cache key
// doesn't pay, since a map key isn't a metric time series.
var ResultCacheRequests = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "result_cache_requests_total",
		Help: "Freshness/result-cache lookups by connector and outcome (hit|miss). result_cache_hit_ratio = rate(hit) / rate(hit+miss).",
	},
	[]string{"connector", "outcome"},
)

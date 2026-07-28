// Package obs holds the metrics this prototype's security claims depend on
// being observable (this file), the real Prometheus histogram GET /metrics
// exposes (prometheus.go), and trace context propagation (tracing.go). The
// in-memory counter/registry below stands in for a full metrics backend
// where no test needs bucketed histograms - just "was a sample recorded,
// under this name and label set."
package obs

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type Counter struct {
	v atomic.Int64
}

func (c *Counter) Inc()         { c.v.Add(1) }
func (c *Counter) Add(n int64)  { c.v.Add(n) }
func (c *Counter) Value() int64 { return c.v.Load() }

// Reset is test-only: EnforcedPredicateViolations is a package-level
// singleton, like a real Prometheus counter would be, so tests that care
// about its baseline must reset it rather than assume a fresh process.
func (c *Counter) Reset() { c.v.Store(0) }

// EnforcedPredicateViolations must stay zero. Non-zero means a connector
// declared a predicate ENFORCED and then didn't actually apply it -
// DESIGN.md ADR-002's "page someone" metric, distinct from
// residual_filter_rows_dropped (expected non-zero, the ADVISORY path's
// normal cost).
var EnforcedPredicateViolations = &Counter{}

// registry is a minimal named+labeled metric store: how many samples were
// recorded, not real histogram buckets or quantiles. The rubric point this
// cycle proves is that a metric exists and is queryable by name and label
// set - not a full latency distribution, which no test asks for.
type registry struct {
	mu      sync.Mutex
	samples map[string]int
}

var metrics = &registry{samples: make(map[string]int)}

// Observe records one sample for name+labels - e.g. one connector
// request's duration. The value itself isn't retained (v1 only counts
// samples); a real system would sum it into bucket counts for
// histogram_quantile-style queries.
func Observe(name string, labels map[string]string, _ float64) {
	key := metricKey(name, labels)
	metrics.mu.Lock()
	metrics.samples[key]++
	metrics.mu.Unlock()
}

// Sample is what Gather returns: just enough to prove a metric was
// recorded at all.
type Sample struct {
	SampleCount int
}

// Gather returns how many samples have been recorded for name+labels.
func Gather(name string, labels map[string]string) Sample {
	key := metricKey(name, labels)
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	return Sample{SampleCount: metrics.samples[key]}
}

// ResetMetrics is test-only: metrics is process-lifetime package state,
// like EnforcedPredicateViolations, so tests that care about an exact
// count rather than just "positive" need a clean baseline.
func ResetMetrics() {
	metrics.mu.Lock()
	metrics.samples = make(map[string]int)
	metrics.mu.Unlock()
}

func metricKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		fmt.Fprintf(&b, ",%s=%s", k, labels[k])
	}
	return b.String()
}

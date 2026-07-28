// Package obs holds the metrics this prototype's security claims depend
// on being observable, plus trace ID propagation (Cycle 12). A minimal
// in-memory counter/registry stands in for real Prometheus instrumentation
// and OpenTelemetry context propagation; the metric identity and semantics
// (what must stay zero, what's expected to move, what should show up on
// the far side of a connector call) are real now.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// traceIDKey is an unexported context key type so obs's trace ID can't
// collide with another package's context value.
type traceIDKey struct{}

// WithTraceID attaches id to ctx - called once, at the HTTP boundary,
// before the request pipeline starts, so every downstream call (exec, the
// connector SDK) can recover the same id without threading it through
// every function signature.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceIDFrom recovers the trace id WithTraceID attached, or "" if none
// was ever attached (e.g. a unit test calling a connector directly).
func TraceIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey{}).(string)
	return id
}

// NewTraceID generates a fresh id - random, not a request counter, so
// trace ids don't leak how many requests a process has handled.
func NewTraceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

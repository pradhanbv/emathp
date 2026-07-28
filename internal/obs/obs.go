// Package obs holds the metrics this prototype's security claims depend
// on being observable. A minimal in-memory counter stands in for real
// Prometheus instrumentation until Cycle 12; the metric identity and
// semantics (what must stay zero, what's expected to move) are real now.
package obs

import "sync/atomic"

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

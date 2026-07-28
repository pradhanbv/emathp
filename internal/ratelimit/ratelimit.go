// Package ratelimit tracks a per-connector call budget.
//
// Real token buckets (DESIGN.md ADR-006) refill over time and are
// Redis-backed so budget is shared and consistent across every gateway
// pod. This is a single-node, non-refilling budget - the README's
// decision register calls this out explicitly as a deliberate MVP
// simplification ("Partial - single-node bucket, 429, async reroute").
// It exists to prove the 429 + Retry-After + async-reroute *paths* exist
// and work, not to model real quota recovery - there is nothing here
// resembling the leased-slice reconciliation or fail-closed-to-local-
// lease behavior the real design specifies for a multi-pod fleet.
package ratelimit

import (
	"fmt"
	"sync"
)

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	limit int
	used  int
}

func New() *Limiter {
	return &Limiter{buckets: make(map[string]*bucket)}
}

// SetLimit configures connector's total call budget for this process's
// lifetime. A connector with no configured limit is unlimited.
func (l *Limiter) SetLimit(connector string, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets[connector] = &bucket{limit: limit}
}

// Allow reports whether connector has budget remaining, consuming one
// unit of it if so.
func (l *Limiter) Allow(connector string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[connector]
	if !ok {
		return true
	}
	if b.used >= b.limit {
		return false
	}
	b.used++
	return true
}

// Remaining reports connector's unused budget, or -1 if it has no
// configured limit (unlimited). Test-only in practice today - the gateway
// itself only needs Allow's boolean - but it's the natural way to assert
// that a specific call spent a token, which freshness revalidation
// (Cycle 9) needs to prove.
func (l *Limiter) Remaining(connector string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[connector]
	if !ok {
		return -1
	}
	return b.limit - b.used
}

// ExhaustedError signals Allow declined a call for connector. Returned by
// callers that wrap Allow's boolean into a richer error path (the
// freshness Source, so a live fetch and a revalidation probe both fail the
// same way a plain fetch used to).
type ExhaustedError struct {
	Connector string
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("rate limit exhausted for connector %q", e.Connector)
}

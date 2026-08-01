// Package freshness caches connector fetches behind a max-staleness budget
// (DESIGN.md ADR-005): a request within budget is served from memory with
// no outbound call; a stale one triggers a conditional (If-None-Match)
// re-fetch that spends rate-limit quota either way, whether the source
// confirms nothing changed or sends fresh rows.
package freshness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/obs"
	"github.com/pradhanbv/emathp/internal/ratelimit"
)

// Cache holds the last fetched rows per distinct outbound request
// signature (principal + table + columns + bound filter values), shared
// across all requests for the gateway process's lifetime. Principal has to
// be part of that signature, not just table/columns/filters: our own
// RLS/CLS don't need it, since a residual filter re-applies on every read
// regardless of cache state - but source-side sharing rules (DESIGN.md
// ADR-002 "layer 3") apply under the calling principal's own delegated
// token, and can differ per user for an identical query independent of
// anything our own policy computes (e.g. Salesforce record-ownership
// sharing, which is per-user, not per-role). A cache keyed on the fetch
// signature alone would silently let one principal's fetch serve rows a
// different principal's own source-side grant would never have returned -
// a leak through the cache that neither the plan-time invariant nor the
// runtime verification filter can see, because neither ever inspects the
// cache.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry

	// Now lets tests fast-forward past a staleness window without a real
	// sleep. Defaults to time.Now.
	Now func() time.Time
}

type entry struct {
	rows      []connector.Row
	etag      string
	fetchedAt time.Time
}

func New() *Cache {
	return &Cache{entries: make(map[string]*entry), Now: time.Now}
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Source wraps a real connector.Source with max-staleness caching. Built
// fresh per request (MaxStaleness varies per query; Cache and RateLimit are
// the long-lived, shared state behind it).
//
// A hit within MaxStaleness returns stored rows: no outbound call, no
// rate-limit spend. A miss or stale entry spends exactly one rate-limit
// token and makes one live call - conditional on the stored ETag when one
// exists, so a confirmed-unchanged response ("revalidated") still counts
// against quota. That's the detail worth getting right: a 304 looks free
// (no new bytes) but a real API bills it as a call, and freshness that
// quietly stops paying for it is a quota leak with good PR.
type Source struct {
	Inner        connector.Source
	Cache        *Cache
	RateLimit    *ratelimit.Limiter
	Connector    string
	Principal    string        // tenant_id + "|" + principal sub - see Cache's doc comment
	MaxStaleness time.Duration // 0 = no caching, always live

	CacheHit    bool
	Revalidated bool
	AgeMS       int64
}

func (s *Source) Fetch(ctx context.Context, req connector.FetchRequest) ([]connector.Row, connector.FetchMeta, error) {
	if s.MaxStaleness <= 0 {
		if !s.RateLimit.Allow(s.Connector) {
			return nil, connector.FetchMeta{}, &ratelimit.ExhaustedError{Connector: s.Connector}
		}
		return s.timedFetch(ctx, req)
	}

	key := cacheKey(req, s.Principal)
	now := s.Cache.now()

	s.Cache.mu.Lock()
	e, ok := s.Cache.entries[key]
	s.Cache.mu.Unlock()

	if ok {
		age := now.Sub(e.fetchedAt)
		if age <= s.MaxStaleness {
			s.CacheHit = true
			s.AgeMS = age.Milliseconds()
			recordResultCacheOutcome(s.Connector, "hit")
			return e.rows, connector.FetchMeta{}, nil
		}
		req.ETag = e.etag
	}
	recordResultCacheOutcome(s.Connector, "miss")

	if !s.RateLimit.Allow(s.Connector) {
		return nil, connector.FetchMeta{}, &ratelimit.ExhaustedError{Connector: s.Connector}
	}

	rows, meta, err := s.timedFetch(ctx, req)
	if err != nil {
		return nil, connector.FetchMeta{}, err
	}

	if meta.NotModified {
		s.Revalidated = true
		s.AgeMS = 0
		e.fetchedAt = now
		s.Cache.mu.Lock()
		s.Cache.entries[key] = e
		s.Cache.mu.Unlock()
		return e.rows, connector.FetchMeta{}, nil
	}

	s.AgeMS = 0
	s.Cache.mu.Lock()
	s.Cache.entries[key] = &entry{rows: rows, etag: meta.ETag, fetchedAt: now}
	s.Cache.mu.Unlock()
	return rows, connector.FetchMeta{}, nil
}

// timedFetch calls Inner.Fetch inside its own "connector.fetch" span - the
// child span a trace viewer renders as the time spent waiting on the
// connector, separate from the gateway's own planning/policy work - and
// records connector_request_duration_seconds regardless of outcome: a
// timeout or a connector error is still a request that took time, and is
// exactly the case observability needs to surface.
func (s *Source) timedFetch(ctx context.Context, req connector.FetchRequest) ([]connector.Row, connector.FetchMeta, error) {
	ctx, span := obs.Tracer.Start(ctx, "connector.fetch", trace.WithAttributes(
		attribute.String("connector", s.Connector),
		attribute.String("table", req.Table),
	))
	defer span.End()

	start := time.Now()
	rows, meta, err := s.Inner.Fetch(ctx, req)
	elapsed := time.Since(start)

	outcome := "success"
	if err != nil {
		outcome = "error"
		span.RecordError(err)
	}

	obs.Observe("connector_request_duration_seconds", map[string]string{"connector": s.Connector}, elapsed.Seconds())
	obs.ConnectorRequestDuration.WithLabelValues(s.Connector, outcome).Observe(elapsed.Seconds())

	return rows, meta, err
}

// cacheKey signs an outbound fetch by principal, table, requested columns,
// and bound filter values (DESIGN_FULL.md ADR-002's result-cache key addendum) -
// the same signature two calls need to share for one to safely serve the
// other's cached rows. principal must be part of it: see Cache's doc
// comment for why table/columns/filters alone isn't enough.
func cacheKey(req connector.FetchRequest, principal string) string {
	cols := append([]string(nil), req.Columns...)
	sort.Strings(cols)

	keys := make([]string, 0, len(req.Filters))
	for k := range req.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|", principal, req.Table, strings.Join(cols, ","))
	for _, k := range keys {
		vals := append([]string(nil), req.Filters[k]...)
		sort.Strings(vals)
		fmt.Fprintf(&b, "%s=%s,", k, strings.Join(vals, "+"))
	}
	return b.String()
}

// recordResultCacheOutcome feeds both the in-memory registry (what tests
// assert against) and the real Prometheus counter (what /metrics exposes)
// from the one call site that actually knows the outcome - the same
// dual-emission pattern timedFetch uses for connector_request_duration_seconds,
// so neither copy can drift from what actually happened.
func recordResultCacheOutcome(connectorName, outcome string) {
	obs.Observe("result_cache_requests_total", map[string]string{"connector": connectorName, "outcome": outcome}, 1)
	obs.ResultCacheRequests.WithLabelValues(connectorName, outcome).Inc()
}

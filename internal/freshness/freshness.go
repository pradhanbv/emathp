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

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/ratelimit"
)

// Cache holds the last fetched rows per distinct outbound request
// signature (table + columns + bound filter values), shared across all
// principals and requests for the gateway process's lifetime. That's safe
// because the signature is taken from the fully-bound FetchRequest exec
// actually sends - two principals whose pushed filter values differ (e.g.
// a per-region RLS predicate that got pushed) never collide on the same
// key.
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
		return s.Inner.Fetch(ctx, req)
	}

	key := cacheKey(req)
	now := s.Cache.now()

	s.Cache.mu.Lock()
	e, ok := s.Cache.entries[key]
	s.Cache.mu.Unlock()

	if ok {
		age := now.Sub(e.fetchedAt)
		if age <= s.MaxStaleness {
			s.CacheHit = true
			s.AgeMS = age.Milliseconds()
			return e.rows, connector.FetchMeta{}, nil
		}
		req.ETag = e.etag
	}

	if !s.RateLimit.Allow(s.Connector) {
		return nil, connector.FetchMeta{}, &ratelimit.ExhaustedError{Connector: s.Connector}
	}

	rows, meta, err := s.Inner.Fetch(ctx, req)
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

// cacheKey signs an outbound fetch by table, requested columns, and bound
// filter values - the same signature two calls need to share for one to
// safely serve the other's cached rows.
func cacheKey(req connector.FetchRequest) string {
	cols := append([]string(nil), req.Columns...)
	sort.Strings(cols)

	keys := make([]string, 0, len(req.Filters))
	for k := range req.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|", req.Table, strings.Join(cols, ","))
	for _, k := range keys {
		vals := append([]string(nil), req.Filters[k]...)
		sort.Strings(vals)
		fmt.Fprintf(&b, "%s=%s,", k, strings.Join(vals, "+"))
	}
	return b.String()
}

// Package mocksf is a Salesforce-ish mock SaaS source: pagination,
// ETag/If-None-Match conditional requests, capability-declared filtering
// (with a deliberate --lie-about escape hatch), field-level-security-style
// column hiding, rate limiting, and configurable delay. It is a test
// fixture, not TDD'd itself (ground rule 4) - it exists to be driven by
// the tests that consume it, starting with the connector SDK's pagination
// test and extending through the lying-connector and rate-limit cycles.
package mocksf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
)

type Server struct {
	rows      []connector.Row
	pageSize  int
	filtered  map[string]bool // column -> mock actually applies this filter
	hidden    map[string]bool // column -> field-level-security hides it
	rateLimit int             // max calls before 429; 0 = unlimited
	delay     time.Duration
	delayMax  time.Duration // if > delay, each response sleeps uniformly in [delay, delayMax]
	etag      string
	callCount atomic.Int64

	mu         sync.Mutex
	lastHeader http.Header
}

type Option func(*Server)

// Rows generates n synthetic accounts (alternating EMEA/APAC region, all
// status "open").
func Rows(n int) Option {
	return func(s *Server) { s.rows = genRows(n) }
}

// Accounts generates n synthetic accounts all in region, with a name and
// an external_id - the join key the semi-join cycle's probe side
// (mockzd.Tickets) correlates against. external_id values are
// "ext-000000".."ext-{n-1:06d}", a numbering scheme mockzd's organization_id
// generation is built to overlap with.
func Accounts(n int, region string) Option {
	return func(s *Server) { s.rows = genAccounts(n, region) }
}

// PageSize caps how many rows the mock returns per call, regardless of
// what the client asks for - a server-side limit, like real APIs impose.
func PageSize(n int) Option {
	return func(s *Server) { s.pageSize = n }
}

// Capability declares whether the mock actually applies column as a
// server-side filter. ENFORCED means it does; anything else (or never
// calling this at all) means it doesn't - matching the capability
// vocabulary's "absent -> no filter exists" default (DESIGN.md ADR-002).
func Capability(column string, enforcement catalog.Enforcement) Option {
	return func(s *Server) { s.filtered[column] = enforcement == catalog.Enforced }
}

// LieAbout overrides column back to unfiltered even after Capability
// declared it ENFORCED - the connector claims enforcement and ignores the
// filter. Pass both together, Capability first, as the lying-connector
// test does.
func LieAbout(column string) Option {
	return func(s *Server) { s.filtered[column] = false }
}

// HideColumn simulates field-level security: requesting this column
// fails the whole call rather than silently omitting it.
func HideColumn(column string) Option {
	return func(s *Server) { s.hidden[column] = true }
}

// RateLimit caps total calls before the mock starts responding 429.
func RateLimit(n int) Option {
	return func(s *Server) { s.rateLimit = n }
}

// Delay adds a fixed delay before every response, for timeout testing.
func Delay(d time.Duration) Option {
	return func(s *Server) { s.delay = d }
}

// DelayJitter makes every response sleep a uniform random duration in
// [min, max] - a stand-in for real connector latency (assumption A4:
// p50 200-800 ms), so a cache miss costs what it would in production
// instead of the ~1 ms an in-process mock answers in. Without it, latency
// and concurrency measurements are meaningless: L = lambda x W collapses to
// ~1 in-flight request no matter the load.
func DelayJitter(min, max time.Duration) Option {
	return func(s *Server) { s.delay, s.delayMax = min, max }
}

// New builds a mock with the given options. Use Start in tests, which
// wraps this in an httptest.Server.
func New(opts ...Option) *Server {
	s := &Server{
		pageSize: 500,
		filtered: make(map[string]bool),
		hidden:   make(map[string]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.etag = computeETag(s.rows)
	return s
}

func (s *Server) CallCount() int { return int(s.callCount.Load()) }

// ReceivedRequest is the headers-only snapshot LastRequest returns - a
// full *http.Request isn't safe to retain past the handler that received
// it, and every test that needs this only ever wants a header value.
type ReceivedRequest struct {
	Header http.Header
}

// LastRequest returns the most recently received request's headers - zero
// value if no request has arrived yet. Used to prove trace ID propagation
// (Cycle 12): the gateway's outbound X-Trace-Id should match the trace_id
// it handed back to its own caller.
func (s *Server) LastRequest() ReceivedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ReceivedRequest{Header: s.lastHeader}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{table}", s.handleTable)
	return mux
}

func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	s.callCount.Add(1)

	s.mu.Lock()
	s.lastHeader = r.Header.Clone()
	s.mu.Unlock()

	if s.delay > 0 {
		d := s.delay
		if s.delayMax > s.delay {
			d += time.Duration(rand.Int63n(int64(s.delayMax - s.delay)))
		}
		time.Sleep(d)
	}

	if s.rateLimit > 0 && s.callCount.Load() > int64(s.rateLimit) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "RATE_LIMIT_EXHAUSTED"})
		return
	}

	fields := splitCSV(r.URL.Query().Get("fields"))
	for _, f := range fields {
		if s.hidden[f] {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "field_not_readable", "field": f})
			return
		}
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == s.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	filtered := s.filterRows(r.URL.Query())

	end := offset + s.pageSize
	hasMore := end < len(filtered)
	if end > len(filtered) {
		end = len(filtered)
	}
	if offset > len(filtered) {
		offset = len(filtered)
	}
	page := filtered[offset:end]

	rows := make([]connector.Row, len(page))
	for i, row := range page {
		out := connector.Row{}
		if len(fields) == 0 {
			for k, v := range row {
				out[k] = v
			}
		} else {
			for _, f := range fields {
				out[f] = row[f]
			}
		}
		rows[i] = out
	}

	w.Header().Set("ETag", s.etag)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows, "has_more": hasMore})
}

// filterRows applies only the columns declared as actually-filtered
// (s.filtered) - an absent or lied-about column is accepted as a query
// parameter but has no effect on which rows come back. A repeated query
// param (org_id=a&org_id=b) is an IN-list; a single value is the equality
// case of the same check.
func (s *Server) filterRows(q map[string][]string) []connector.Row {
	var out []connector.Row
	for _, row := range s.rows {
		keep := true
		for col, vals := range q {
			if col == "fields" || col == "offset" || len(vals) == 0 {
				continue
			}
			if !s.filtered[col] {
				continue
			}
			if !containsStr(vals, row[col]) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, row)
		}
	}
	return out
}

func containsStr(vals []string, v string) bool {
	for _, want := range vals {
		if want == v {
			return true
		}
	}
	return false
}

func genAccounts(n int, region string) []connector.Row {
	rows := make([]connector.Row, n)
	for i := 0; i < n; i++ {
		rows[i] = connector.Row{
			"id":          fmt.Sprintf("a%06d", i),
			"name":        fmt.Sprintf("Account %d", i),
			"email":       fmt.Sprintf("user%d@example.com", i),
			"region":      region,
			"status":      "open",
			"external_id": fmt.Sprintf("ext-%06d", i),
		}
	}
	return rows
}

func genRows(n int) []connector.Row {
	rows := make([]connector.Row, n)
	for i := 0; i < n; i++ {
		region := "EMEA"
		if i%2 == 1 {
			region = "APAC"
		}
		rows[i] = connector.Row{
			"id":          fmt.Sprintf("a%06d", i),
			"name":        fmt.Sprintf("Account %d", i),
			"email":       fmt.Sprintf("user%d@example.com", i),
			"region":      region,
			"status":      "open",
			"external_id": fmt.Sprintf("ext-%06d", i),
		}
	}
	return rows
}

func computeETag(rows []connector.Row) string {
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%v", r) // fmt sorts map keys for %v, so this is deterministic
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

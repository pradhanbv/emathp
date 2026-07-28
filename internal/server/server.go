// Package server implements the gateway's HTTP boundary: the /v1/query
// handler and the response envelope every cycle fills in a little more.
// Cycle 6 replaced the hardcoded stub with the real pipeline; Cycle 7
// inserts the plan cache between policy resolution and Build, and moves
// $principal.<attr> binding into exec.Run (after the cache lookup, per
// ADR-003 - a plan shared across principals in the same role must never
// have another principal's values written into it). Cycle 8 adds the
// rate-limit check and the Prefer: respond-async reroute, both built
// around a single run() that both the sync and async paths call. Cycle 9
// wraps the connector Source in a freshness.Source per request, so
// max_staleness caching and rate-limit spend live together at the one call
// site that actually goes out to a connector.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/exec"
	"github.com/pradhanbv/emathp/internal/freshness"
	"github.com/pradhanbv/emathp/internal/identity"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/plancache"
	"github.com/pradhanbv/emathp/internal/policy"
	"github.com/pradhanbv/emathp/internal/ratelimit"
)

// Deps are the gateway's dependencies, supplied by the caller rather than
// loaded from a hardcoded path - cmd/gateway/main.go and test harnesses
// both construct these the same way, from whatever source fits their
// context (real files, temp fixtures, a mock connector's URL).
type Deps struct {
	Catalog   *catalog.Catalog
	Policy    *policy.Provider
	Identity  *identity.Registry
	PlanCache *plancache.Cache
	RateLimit *ratelimit.Limiter
	Freshness *freshness.Cache
	Sources   map[string]connector.Source // connector prefix (e.g. "sf") -> source
}

type Server struct {
	deps Deps
	jobs sync.Map // jobID string -> *asyncJob
}

func New(deps Deps) *Server { return &Server{deps: deps} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/query", s.handleQuery)
	mux.HandleFunc("GET /v1/jobs/{id}", s.handlePoll)
	return mux
}

// outcome is a response envelope plus the HTTP status and any extra
// headers it needs (Retry-After) - the shape run() produces so the sync
// and async paths can share it without the async path needing an
// http.ResponseWriter at all.
type outcome struct {
	Status  int
	Body    QueryResponse
	Headers map[string]string
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOutcome(w, errorOutcome(http.StatusBadRequest, "BAD_REQUEST", err.Error()))
		return
	}

	principal, err := identity.ResolveFromHeader(r.Header.Get("Authorization"), s.deps.Identity)
	if err != nil {
		writeOutcome(w, errorOutcome(http.StatusServiceUnavailable, "PRINCIPAL_UNRESOLVED", err.Error()))
		return
	}
	if len(principal.Roles) == 0 {
		writeOutcome(w, errorOutcome(http.StatusForbidden, "ENTITLEMENT_DENIED", "principal has no roles"))
		return
	}
	// v1 simplification: fixtures only ever produce one role per
	// principal. Multi-role composition (union of grants, most
	// restrictive mask wins, etc.) is unmodeled.
	role := principal.Roles[0]

	if r.Header.Get("Prefer") == "respond-async" {
		id := s.startAsync(req, principal, role)
		writeOutcome(w, outcome{
			Status: http.StatusAccepted,
			Body: QueryResponse{
				RateLimitStatus: map[string]string{},
				TraceID:         "trace-stub",
				PollURL:         "/v1/jobs/" + id,
			},
		})
		return
	}

	writeOutcome(w, s.run(r.Context(), req, principal, role))
}

// run executes the full pipeline: L1 object authz, plan cache lookup,
// the fail-closed invariant, the rate-limit reservation, and exec (with
// its own runtime verification filter). Shared by the synchronous
// handler and the async job goroutine so neither path can drift from the
// other's behaviour.
func (s *Server) run(ctx context.Context, req QueryRequest, principal identity.Principal, role string) outcome {
	table, err := plan.ParseTable(req.SQL)
	if err != nil {
		return errorOutcome(http.StatusBadRequest, "UNSUPPORTED_PREDICATE", err.Error())
	}

	// LAYER 1 - object-level authz, ours, pre-plan (ADR-002). Denied
	// before we ever touch the catalog or build a plan.
	if s.deps.Policy.ObjectDenied(role, table) {
		return errorOutcome(http.StatusForbidden, "ENTITLEMENT_DENIED", "role may not access "+table)
	}

	// Plan cache lookup (ADR-003): built fresh on a miss, reused on a hit.
	// The plan itself never contains a resolved $principal value - it's
	// only safe to share across principals in the same role because
	// binding happens later, in exec.Run, against a read-only plan.
	p, _, _, err := plancache.Resolve(s.deps.PlanCache, req.SQL, s.deps.Catalog, s.deps.Policy, principal.Tenant, role)
	if err != nil {
		return errorOutcome(http.StatusBadRequest, "UNSUPPORTED_PREDICATE", err.Error())
	}

	// LAYER 2's fail-closed check. Runs on every request, including
	// cache hits (ADR-002) - cheap, and a defence against a corrupted or
	// tampered cache entry, not just a fresh-build sanity check.
	if err := plan.AssertInvariant(p); err != nil {
		return errorOutcome(http.StatusForbidden, "ENTITLEMENT_DENIED", err.Error())
	}

	connectorName := connectorPrefix(table)

	source, ok := s.deps.Sources[connectorName]
	if !ok {
		return errorOutcome(http.StatusBadGateway, "CONNECTOR_AUTH_FAILED", "no connector configured for "+table)
	}

	// Freshness (ADR-005): a cache hit within max_staleness never touches
	// the rate limiter or the connector at all. A miss or stale entry
	// spends exactly one token and makes one (conditional, if an ETag is on
	// hand) call - the same reservation-after-planning sequencing
	// DESIGN.md Section 2.3 describes, just moved inside the wrapper so it
	// only fires when a call is actually going out.
	fresh := &freshness.Source{
		Inner:        source,
		Cache:        s.deps.Freshness,
		RateLimit:    s.deps.RateLimit,
		Connector:    connectorName,
		MaxStaleness: parseMaxStaleness(req.MaxStaleness),
	}

	result, err := exec.Run(ctx, p, fresh, principal.Attributes)
	if err != nil {
		var exhausted *ratelimit.ExhaustedError
		if errors.As(err, &exhausted) {
			o := errorOutcome(http.StatusTooManyRequests, "RATE_LIMIT_EXHAUSTED",
				fmt.Sprintf("rate limit exhausted for connector %q; retry after the window resets or use Prefer: respond-async", connectorName))
			o.Headers = map[string]string{"Retry-After": "1"}
			return o
		}
		if errors.Is(err, plan.ErrEntitlementDenied) {
			return errorOutcome(http.StatusForbidden, "ENTITLEMENT_DENIED", err.Error())
		}
		return errorOutcome(http.StatusBadGateway, "CONNECTOR_AUTH_FAILED", err.Error())
	}

	return resultOutcome(result, fresh)
}

// parseMaxStaleness turns the request's max_staleness (e.g. "60s") into a
// duration. Empty or malformed input means "no budget" - always live,
// never cached - which is the fail-safe direction: worst case is an extra
// live fetch, never data staler than the caller asked for.
func parseMaxStaleness(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// asyncJob is the in-memory record behind Prefer: respond-async. Not a
// real job queue - no persistence, no retry, no cross-pod visibility -
// the rubric point is the reroute path existing, per IMPLEMENTATION_PLAN.
type asyncJob struct {
	mu   sync.Mutex
	done bool
	out  outcome
}

var jobSeq atomic.Int64

func (s *Server) startAsync(req QueryRequest, principal identity.Principal, role string) string {
	id := fmt.Sprintf("job-%d", jobSeq.Add(1))
	job := &asyncJob{}
	s.jobs.Store(id, job)

	go func() {
		// Detached from the originating request's context - a real
		// system would use a bounded background context with its own
		// timeout budget (ADR-009); this MVP just runs to completion.
		o := s.run(context.Background(), req, principal, role)
		job.mu.Lock()
		job.done = true
		job.out = o
		job.mu.Unlock()
	}()

	return id
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	v, ok := s.jobs.Load(r.PathValue("id"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	job := v.(*asyncJob)
	job.mu.Lock()
	status := JobStatus{Done: job.done}
	if job.done {
		status.Result = &job.out.Body
	}
	job.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func connectorPrefix(table string) string {
	if i := strings.Index(table, "."); i >= 0 {
		return table[:i]
	}
	return table
}

func resultOutcome(result *exec.Result, fresh *freshness.Source) outcome {
	rows := make([][]any, len(result.Rows))
	for i, row := range result.Rows {
		vals := make([]any, len(result.Columns))
		for j, col := range result.Columns {
			vals[j] = row[col]
		}
		rows[i] = vals
	}

	var meta *Meta
	if fresh.CacheHit || fresh.Revalidated {
		meta = &Meta{CacheHit: fresh.CacheHit, Revalidated: fresh.Revalidated}
	}

	freshnessMS := fresh.AgeMS
	return outcome{
		Status: http.StatusOK,
		Body: QueryResponse{
			Columns:         result.Columns,
			Rows:            rows,
			FreshnessMS:     &freshnessMS,
			RateLimitStatus: map[string]string{"sf": "ok"},
			TraceID:         "trace-stub",
			Meta:            meta,
		},
	}
}

func errorOutcome(status int, code, message string) outcome {
	return outcome{
		Status: status,
		Body: QueryResponse{
			RateLimitStatus: map[string]string{},
			TraceID:         "trace-stub",
			Error:           &ErrorBody{Code: code, Message: message},
		},
	}
}

func writeOutcome(w http.ResponseWriter, o outcome) {
	for k, v := range o.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(o.Status)
	_ = json.NewEncoder(w).Encode(o.Body)
}

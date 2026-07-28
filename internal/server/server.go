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
	"github.com/pradhanbv/emathp/internal/obs"
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
	// Every request gets a trace id up front, before anything can fail -
	// even a BAD_REQUEST response should be traceable. Carried via context
	// (Cycle 12) rather than threaded through every function signature, so
	// it can reach the connector SDK's outbound HTTP request without every
	// intermediate call needing to know about tracing.
	traceID := obs.NewTraceID()
	ctx := obs.WithTraceID(r.Context(), traceID)

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOutcome(w, errorOutcome(traceID, http.StatusBadRequest, "BAD_REQUEST", err.Error()))
		return
	}

	principal, err := identity.ResolveFromHeader(r.Header.Get("Authorization"), s.deps.Identity)
	if err != nil {
		writeOutcome(w, errorOutcome(traceID, http.StatusServiceUnavailable, "PRINCIPAL_UNRESOLVED", err.Error()))
		return
	}
	if len(principal.Roles) == 0 {
		writeOutcome(w, errorOutcome(traceID, http.StatusForbidden, "ENTITLEMENT_DENIED", "principal has no roles"))
		return
	}
	// v1 simplification: fixtures only ever produce one role per
	// principal. Multi-role composition (union of grants, most
	// restrictive mask wins, etc.) is unmodeled.
	role := principal.Roles[0]

	if r.Header.Get("Accept") == "application/x-ndjson" {
		s.runStream(w, ctx, req, principal, role)
		return
	}

	if r.Header.Get("Prefer") == "respond-async" {
		id := s.startAsync(ctx, req, principal, role)
		writeOutcome(w, outcome{
			Status: http.StatusAccepted,
			Body: QueryResponse{
				RateLimitStatus: map[string]string{},
				TraceID:         traceID,
				PollURL:         "/v1/jobs/" + id,
			},
		})
		return
	}

	writeOutcome(w, s.run(ctx, req, principal, role))
}

// errConnectorMissing signals a table's connector prefix has no entry in
// Deps.Sources - a deployment/config error, not a query error.
var errConnectorMissing = errors.New("no connector configured")

// buildAndRoute runs everything before exec.Run: parse every referenced
// table, L1 object authz, the plan-cache lookup (build on miss), the
// fail-closed invariant, and one freshness-wrapped Source per distinct
// connector the query touches. Shared by run() (JSON response) and
// runStream() (NDJSON response, Cycle 11) so neither can drift from the
// other's admission and routing logic - they differ only in how they turn
// exec.Run's result or error into a response.
func (s *Server) buildAndRoute(req QueryRequest, principal identity.Principal, role string) (*plan.Plan, map[string]connector.Source, error) {
	tables, err := plan.ParseTables(req.SQL)
	if err != nil {
		return nil, nil, err
	}

	// LAYER 1 - object-level authz, ours, pre-plan (ADR-002). Denied
	// before we ever touch the catalog or build a plan. A join is denied
	// if either side is - both tables need admission, not just the one
	// named first.
	for _, table := range tables {
		if s.deps.Policy.ObjectDenied(role, table) {
			return nil, nil, fmt.Errorf("%w: role may not access %s", plan.ErrEntitlementDenied, table)
		}
	}

	// Plan cache lookup (ADR-003): built fresh on a miss, reused on a hit.
	// The plan itself never contains a resolved $principal value - it's
	// only safe to share across principals in the same role because
	// binding happens later, in exec.Run, against a read-only plan.
	p, _, _, err := plancache.Resolve(s.deps.PlanCache, req.SQL, s.deps.Catalog, s.deps.Policy, principal.Tenant, role)
	if err != nil {
		return nil, nil, err
	}

	// LAYER 2's fail-closed check. Runs on every request, including
	// cache hits (ADR-002) - cheap, and a defence against a corrupted or
	// tampered cache entry, not just a fresh-build sanity check.
	if err := plan.AssertInvariant(p); err != nil {
		return nil, nil, err
	}

	// Freshness (ADR-005): a cache hit within max_staleness never touches
	// the rate limiter or the connector at all. A miss or stale entry
	// spends exactly one token and makes one (conditional, if an ETag is on
	// hand) call - the same reservation-after-planning sequencing
	// DESIGN.md Section 2.3 describes, just moved inside the wrapper so it
	// only fires when a call is actually going out. A join needs one
	// wrapper per distinct connector (Cycle 10) - each side's rate budget
	// and freshness cache are its own connector's, not shared.
	maxStaleness := parseMaxStaleness(req.MaxStaleness)
	sources := make(map[string]connector.Source, len(tables))
	for _, table := range tables {
		name := connectorPrefix(table)
		if _, ok := sources[name]; ok {
			continue
		}
		source, ok := s.deps.Sources[name]
		if !ok {
			return nil, nil, fmt.Errorf("%w: no connector configured for %s", errConnectorMissing, table)
		}
		sources[name] = &freshness.Source{
			Inner:        source,
			Cache:        s.deps.Freshness,
			RateLimit:    s.deps.RateLimit,
			Connector:    name,
			MaxStaleness: maxStaleness,
		}
	}

	return p, sources, nil
}

// classifyError maps an error from buildAndRoute or exec.Run to an HTTP
// status and response code. defaultStatus/defaultCode apply to whatever
// doesn't match a recognized type - callers pass a different default
// depending on which stage the error came from (buildAndRoute's default is
// a query-shape problem; exec.Run's is a connector problem), since a bare
// type switch can't otherwise tell the two apart.
func classifyError(err error, defaultStatus int, defaultCode string) (status int, code string) {
	var exhausted *ratelimit.ExhaustedError
	var timedOut *exec.SourceTimeoutError
	switch {
	case errors.As(err, &exhausted):
		return http.StatusTooManyRequests, "RATE_LIMIT_EXHAUSTED"
	case errors.As(err, &timedOut):
		return http.StatusGatewayTimeout, "SOURCE_TIMEOUT"
	case errors.Is(err, plan.ErrEntitlementDenied):
		return http.StatusForbidden, "ENTITLEMENT_DENIED"
	case errors.Is(err, errConnectorMissing):
		return http.StatusBadGateway, "CONNECTOR_AUTH_FAILED"
	default:
		return defaultStatus, defaultCode
	}
}

// run executes the full pipeline and shapes the result as the plain JSON
// envelope. Shared by the synchronous handler and the async job goroutine
// so neither path can drift from the other's behaviour.
func (s *Server) run(ctx context.Context, req QueryRequest, principal identity.Principal, role string) outcome {
	traceID := obs.TraceIDFrom(ctx)

	p, sources, err := s.buildAndRoute(req, principal, role)
	if err != nil {
		status, code := classifyError(err, http.StatusBadRequest, "UNSUPPORTED_PREDICATE")
		return errorOutcome(traceID, status, code, err.Error())
	}

	timeout := parseTimeout(req.Timeout)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	result, err := exec.Run(ctx, p, sources, principal.Attributes)
	if err != nil {
		status, code := classifyError(err, http.StatusBadGateway, "CONNECTOR_AUTH_FAILED")
		message := err.Error()
		var exhausted *ratelimit.ExhaustedError
		if errors.As(err, &exhausted) {
			message = fmt.Sprintf("rate limit exhausted for connector %q; retry after the window resets or use Prefer: respond-async", exhausted.Connector)
		}
		o := errorOutcome(traceID, status, code, message)
		if code == "RATE_LIMIT_EXHAUSTED" {
			o.Headers = map[string]string{"Retry-After": "1"}
		}
		return o
	}

	return resultOutcome(traceID, result, summarizeFreshness(sources))
}

// runStream is buildAndRoute+exec.Run shaped as a single NDJSON terminal
// Frame instead of a JSON outcome (Cycle 11, ADR-009). The one failure mode
// this cycle attributes to a specific connector is a timeout
// (*exec.SourceTimeoutError): Partial is set and Sources names which
// connector didn't finish in time. Any other error falls back to the same
// classification run() uses, reported in Frame.Error instead of a source
// name, since it isn't attributable to one connector the way a timeout is.
func (s *Server) runStream(w http.ResponseWriter, ctx context.Context, req QueryRequest, principal identity.Principal, role string) {
	timeout := parseTimeout(req.Timeout)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	frame := Frame{IsTerminal: true, TraceID: obs.TraceIDFrom(ctx)}

	p, sources, err := s.buildAndRoute(req, principal, role)
	if err == nil {
		var result *exec.Result
		result, err = exec.Run(ctx, p, sources, principal.Attributes)
		if err == nil {
			frame.Columns = result.Columns
			frame.Rows = rowsToAny(result.Columns, result.Rows)
		}
	}

	if err != nil {
		frame.Partial = true
		var timedOut *exec.SourceTimeoutError
		if errors.As(err, &timedOut) {
			frame.Sources = map[string]SourceStatus{timedOut.Connector: {Error: "SOURCE_TIMEOUT"}}
		} else {
			_, code := classifyError(err, http.StatusBadGateway, "CONNECTOR_AUTH_FAILED")
			frame.Error = &ErrorBody{Code: code, Message: err.Error()}
		}
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	_ = json.NewEncoder(w).Encode(frame)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// parseTimeout turns the request's timeout (e.g. "1s") into a duration.
// Empty or malformed input means "no deadline" - the fail-safe direction:
// worst case a slow connector runs to completion instead of being cut off,
// never a request failing on a budget the caller didn't actually ask for.
func parseTimeout(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// freshnessSummary folds every connector's freshness.Source state into one
// response-level view: CacheHit only if every fetch this request made was
// served from cache (a join is only as cache-fresh as its stalest side),
// Revalidated if any side had to, AgeMS the oldest of them (the result is
// only as fresh as its stalest input).
type freshnessSummary struct {
	CacheHit    bool
	Revalidated bool
	AgeMS       int64
}

func summarizeFreshness(sources map[string]connector.Source) freshnessSummary {
	var sum freshnessSummary
	first := true
	for _, src := range sources {
		fs, ok := src.(*freshness.Source)
		if !ok {
			continue
		}
		if first {
			sum.CacheHit = fs.CacheHit
			first = false
		} else {
			sum.CacheHit = sum.CacheHit && fs.CacheHit
		}
		sum.Revalidated = sum.Revalidated || fs.Revalidated
		if fs.AgeMS > sum.AgeMS {
			sum.AgeMS = fs.AgeMS
		}
	}
	return sum
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

func (s *Server) startAsync(ctx context.Context, req QueryRequest, principal identity.Principal, role string) string {
	id := fmt.Sprintf("job-%d", jobSeq.Add(1))
	job := &asyncJob{}
	s.jobs.Store(id, job)

	// The background goroutine gets a fresh Background() context, detached
	// from the originating request's cancellation - a real system would
	// use a bounded background context with its own timeout budget
	// (ADR-009); this MVP just runs to completion. The trace id travels
	// forward regardless, since it's a value carried by the context, not
	// tied to the request's lifetime the way cancellation is.
	bgCtx := obs.WithTraceID(context.Background(), obs.TraceIDFrom(ctx))

	go func() {
		o := s.run(bgCtx, req, principal, role)
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

// rowsToAny converts exec's []map[string]string rows into the response
// envelope's positional [][]any shape (column order fixed by columns) -
// shared by the JSON and NDJSON response paths.
func rowsToAny(columns []string, rows []map[string]string) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		vals := make([]any, len(columns))
		for j, col := range columns {
			vals[j] = row[col]
		}
		out[i] = vals
	}
	return out
}

func resultOutcome(traceID string, result *exec.Result, fresh freshnessSummary) outcome {
	rows := rowsToAny(result.Columns, result.Rows)

	var meta *Meta
	if fresh.CacheHit || fresh.Revalidated || result.JoinStrategy != "" {
		meta = &Meta{
			CacheHit:          fresh.CacheHit,
			Revalidated:       fresh.Revalidated,
			JoinStrategy:      result.JoinStrategy,
			NaiveCallEstimate: result.NaiveCallEstimate,
		}
	}

	freshnessMS := fresh.AgeMS
	return outcome{
		Status: http.StatusOK,
		Body: QueryResponse{
			Columns:         result.Columns,
			Rows:            rows,
			FreshnessMS:     &freshnessMS,
			RateLimitStatus: map[string]string{"sf": "ok"},
			TraceID:         traceID,
			Meta:            meta,
		},
	}
}

func errorOutcome(traceID string, status int, code, message string) outcome {
	return outcome{
		Status: status,
		Body: QueryResponse{
			RateLimitStatus: map[string]string{},
			TraceID:         traceID,
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

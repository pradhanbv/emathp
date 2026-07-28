// Package server implements the gateway's HTTP boundary: the /v1/query
// handler and the response envelope every cycle fills in a little more.
// Cycle 6 replaces the hardcoded stub with the real pipeline: resolve the
// principal, check L1 object authorization, compile policy into a plan,
// assert the fail-closed invariant, execute (with the runtime
// verification filter), map the result or a domain error onto the
// envelope.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/exec"
	"github.com/pradhanbv/emathp/internal/identity"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/policy"
)

// Deps are the gateway's dependencies, supplied by the caller rather than
// loaded from a hardcoded path - cmd/gateway/main.go and test harnesses
// both construct these the same way, from whatever source fits their
// context (real files, temp fixtures, a mock connector's URL).
type Deps struct {
	Catalog  *catalog.Catalog
	Policy   *policy.Provider
	Identity *identity.Registry
	Sources  map[string]connector.Source // connector prefix (e.g. "sf") -> source
}

type Server struct {
	deps Deps
}

func New(deps Deps) *Server { return &Server{deps: deps} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/query", s.handleQuery)
	return mux
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	principal, err := identity.ResolveFromHeader(r.Header.Get("Authorization"), s.deps.Identity)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "PRINCIPAL_UNRESOLVED", err.Error())
		return
	}
	if len(principal.Roles) == 0 {
		writeError(w, http.StatusForbidden, "ENTITLEMENT_DENIED", "principal has no roles")
		return
	}
	// v1 simplification: fixtures only ever produce one role per
	// principal. Multi-role composition (union of grants, most
	// restrictive mask wins, etc.) is unmodeled.
	role := principal.Roles[0]

	table, err := plan.ParseTable(req.SQL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_PREDICATE", err.Error())
		return
	}

	// LAYER 1 - object-level authz, ours, pre-plan (ADR-002). Denied
	// before we ever touch the catalog or build a plan.
	if s.deps.Policy.ObjectDenied(role, table) {
		writeError(w, http.StatusForbidden, "ENTITLEMENT_DENIED", "role may not access "+table)
		return
	}

	residuals, err := s.deps.Policy.ResidualsFor(role, table)
	if err != nil {
		writeError(w, http.StatusForbidden, "ENTITLEMENT_DENIED", err.Error())
		return
	}
	residuals, err = policy.BindResiduals(residuals, principal.Attributes)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "PRINCIPAL_UNRESOLVED", err.Error())
		return
	}

	masks, err := s.deps.Policy.MasksFor(role, table)
	if err != nil {
		writeError(w, http.StatusForbidden, "ENTITLEMENT_DENIED", err.Error())
		return
	}

	p, err := plan.Build(req.SQL, s.deps.Catalog, residuals, masks)
	if err != nil {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_PREDICATE", err.Error())
		return
	}

	// LAYER 2's fail-closed check, after parameter binding (ADR-002).
	if err := plan.AssertInvariant(p); err != nil {
		writeError(w, http.StatusForbidden, "ENTITLEMENT_DENIED", err.Error())
		return
	}

	source, ok := s.deps.Sources[connectorPrefix(table)]
	if !ok {
		writeError(w, http.StatusBadGateway, "CONNECTOR_AUTH_FAILED", "no connector configured for "+table)
		return
	}

	result, err := exec.Run(r.Context(), p, source)
	if err != nil {
		if errors.Is(err, plan.ErrEntitlementDenied) {
			writeError(w, http.StatusForbidden, "ENTITLEMENT_DENIED", err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "CONNECTOR_AUTH_FAILED", err.Error())
		return
	}

	writeResult(w, result)
}

func connectorPrefix(table string) string {
	if i := strings.Index(table, "."); i >= 0 {
		return table[:i]
	}
	return table
}

func writeResult(w http.ResponseWriter, result *exec.Result) {
	rows := make([][]any, len(result.Rows))
	for i, row := range result.Rows {
		vals := make([]any, len(result.Columns))
		for j, col := range result.Columns {
			vals[j] = row[col]
		}
		rows[i] = vals
	}

	freshness := int64(0)
	resp := QueryResponse{
		Columns:         result.Columns,
		Rows:            rows,
		FreshnessMS:     &freshness,
		RateLimitStatus: map[string]string{"sf": "ok"},
		TraceID:         "trace-stub",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	resp := QueryResponse{
		RateLimitStatus: map[string]string{},
		TraceID:         "trace-stub",
		Error:           &ErrorBody{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// Package server implements the gateway's HTTP boundary: the /v1/query
// handler and the response envelope every later cycle fills in.
package server

import (
	"encoding/json"
	"net/http"
)

type Server struct{}

func New() *Server { return &Server{} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/query", s.handleQuery)
	return mux
}

// handleQuery is a stub: hardcoded rows, real envelope shape. Cycle 1+
// replaces the body with identity resolution, planning, and execution.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	freshness := int64(0)
	resp := QueryResponse{
		Columns:         []string{"id"},
		Rows:            [][]any{{"a001"}},
		FreshnessMS:     &freshness,
		RateLimitStatus: map[string]string{"sf": "ok"},
		TraceID:         "trace-stub",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

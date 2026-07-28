// Package mockzd is a Zendesk-ish mock SaaS source: the semi-join cycle's
// probe side. Its one load-bearing behaviour is MaxInList - rejecting an
// organization_id IN-list longer than the connector declares it accepts,
// the same way a real ticketing API caps a batch lookup's key count. Like
// mocksf, this is a test fixture (IMPLEMENTATION_PLAN.md ground rule 4),
// not TDD'd itself.
package mockzd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/pradhanbv/emathp/internal/connector"
)

type Server struct {
	tickets   []connector.Row
	pageSize  int
	maxInList int
	callCount atomic.Int64
}

type Option func(*Server)

// Tickets generates n synthetic tickets, all with the given status, whose
// organization_id cycles over a 10,000-value id space ("ext-000000"..
// "ext-009999") using the same "ext-%06d" scheme mocksf.Accounts uses for
// external_id - the two mocks are independent processes with no shared
// state, so the join key overlap has to come from a shared, deterministic
// naming convention rather than any actual coordination.
func Tickets(n int, status string) Option {
	return func(s *Server) { s.tickets = genTickets(n, status) }
}

// MaxInList caps how many organization_id values one request may filter
// on; a request over the limit is rejected rather than silently
// truncated, so a gateway that fails to chunk fails the test loudly
// instead of quietly returning a partial (and therefore wrong) result.
func MaxInList(n int) Option {
	return func(s *Server) { s.maxInList = n }
}

// New builds a mock with the given options. pageSize defaults large enough
// that one semi-join chunk's matching tickets fit in a single page for any
// fixture size this mock is meant to exercise - pagination itself is
// already proven by mocksf's Cycle 5 test; this mock's job is proving the
// chunking mechanism, not re-proving pagination.
func New(opts ...Option) *Server {
	s := &Server{pageSize: 5000}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) CallCount() int { return int(s.callCount.Load()) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{table}", s.handleTable)
	return mux
}

func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	s.callCount.Add(1)

	q := r.URL.Query()

	if orgIDs, ok := q["organization_id"]; ok && s.maxInList > 0 && len(orgIDs) > s.maxInList {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "IN_LIST_TOO_LARGE"})
		return
	}

	fields := splitCSV(q.Get("fields"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	filtered := s.filterRows(q)

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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows, "has_more": hasMore})
}

// filterRows applies every recognized filter column as an IN-list check -
// a single value is just the equality case of the same check. Unlike
// mocksf, every predicate this mock accepts is actually enforced: there's
// no capability-lying scenario to model on the probe side for this cycle.
func (s *Server) filterRows(q map[string][]string) []connector.Row {
	var out []connector.Row
	for _, row := range s.tickets {
		keep := true
		for col, vals := range q {
			if col == "fields" || col == "offset" || len(vals) == 0 {
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

const orgSpace = 10000

func genTickets(n int, status string) []connector.Row {
	rows := make([]connector.Row, n)
	for i := 0; i < n; i++ {
		rows[i] = connector.Row{
			"id":              fmt.Sprintf("t%06d", i),
			"subject":         fmt.Sprintf("Ticket %d", i),
			"status":          status,
			"organization_id": fmt.Sprintf("ext-%06d", i%orgSpace),
		}
	}
	return rows
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

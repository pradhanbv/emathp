// Package harness starts an in-process gateway and drives it through HTTP,
// keeping acceptance tests black-box.
package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pradhanbv/emathp/internal/server"
)

type Gateway struct {
	srv *httptest.Server
	t   *testing.T
}

func Start(t *testing.T, deps server.Deps) *Gateway {
	t.Helper()
	s := server.New(deps)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &Gateway{srv: ts, t: t}
}

// ReqOption mutates an outgoing request before it's sent.
type ReqOption func(*http.Request)

// Token attaches a bearer token for one of the fixture identities
// (currently "support" and "admin"). See testdata/tokens.
func Token(role string) ReqOption {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tokenFor(role))
	}
}

type Response struct {
	Code   int
	Body   server.QueryResponse
	Header http.Header
}

func (g *Gateway) POST(path, body string, opts ...ReqOption) *Response {
	g.t.Helper()

	req, err := http.NewRequest(http.MethodPost, g.srv.URL+path, bytes.NewBufferString(body))
	if err != nil {
		g.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var qr server.QueryResponse
	_ = json.NewDecoder(resp.Body).Decode(&qr)

	return &Response{Code: resp.StatusCode, Body: qr, Header: resp.Header}
}

// Query is the common-case convenience over POST: build the JSON body
// from sql, attach persona's token.
func (g *Gateway) Query(persona, sql string, opts ...ReqOption) *Response {
	body := fmt.Sprintf(`{"sql":%q}`, sql)
	allOpts := append([]ReqOption{Token(persona)}, opts...)
	return g.POST("/v1/query", body, allOpts...)
}

// QueryWithHeader is Query plus one extra request header - e.g.
// Prefer: respond-async.
func (g *Gateway) QueryWithHeader(persona, sql, header, value string) *Response {
	return g.Query(persona, sql, func(r *http.Request) {
		r.Header.Set(header, value)
	})
}

// QueryFresh is Query plus a max_staleness budget (e.g. "60s") in the
// request body, for the freshness-cache tests (ADR-005).
func (g *Gateway) QueryFresh(persona, sql, maxStaleness string, opts ...ReqOption) *Response {
	body := fmt.Sprintf(`{"sql":%q,"max_staleness":%q}`, sql, maxStaleness)
	allOpts := append([]ReqOption{Token(persona)}, opts...)
	return g.POST("/v1/query", body, allOpts...)
}

// StreamResponse is a decoded `Accept: application/x-ndjson` response - one
// or more NDJSON lines, the last of which is terminal (Cycle 11, ADR-009).
type StreamResponse struct {
	Code   int
	Header http.Header
	body   []byte
}

// QueryStream is Query for the NDJSON streaming path: sets
// Accept: application/x-ndjson and an optional timeout (e.g. "1s") in the
// request body.
func (g *Gateway) QueryStream(persona, sql, timeout string) *StreamResponse {
	g.t.Helper()

	body := fmt.Sprintf(`{"sql":%q,"timeout":%q}`, sql, timeout)
	req, err := http.NewRequest(http.MethodPost, g.srv.URL+"/v1/query", bytes.NewBufferString(body))
	if err != nil {
		g.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	Token(persona)(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		g.t.Fatalf("read body: %v", err)
	}

	return &StreamResponse{Code: resp.StatusCode, Header: resp.Header, body: data}
}

// NDJSON decodes the response body as newline-delimited Frame objects.
func (r *StreamResponse) NDJSON() []server.Frame {
	var frames []server.Frame
	scanner := bufio.NewScanner(bytes.NewReader(r.body))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var f server.Frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		frames = append(frames, f)
	}
	return frames
}

// PollResult is the /v1/jobs/{id} response an async query's PollURL
// resolves to.
type PollResult struct {
	Code int
	Done bool
	Body server.QueryResponse
}

func (g *Gateway) Poll(path string) *PollResult {
	g.t.Helper()

	resp, err := http.Get(g.srv.URL + path)
	if err != nil {
		g.t.Fatalf("poll %s: %v", path, err)
	}
	defer resp.Body.Close()

	var status server.JobStatus
	_ = json.NewDecoder(resp.Body).Decode(&status)

	pr := &PollResult{Code: resp.StatusCode, Done: status.Done}
	if status.Result != nil {
		pr.Body = *status.Result
	}
	return pr
}

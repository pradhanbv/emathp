// Package harness starts an in-process gateway and drives it through HTTP,
// keeping acceptance tests black-box.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
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

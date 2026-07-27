// Package harness starts an in-process gateway and drives it through HTTP,
// keeping acceptance tests black-box.
package harness

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pradhanbv/emathp/internal/server"
)

type Gateway struct {
	srv *httptest.Server
	t   *testing.T
}

func Start(t *testing.T) *Gateway {
	t.Helper()
	s := server.New()
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

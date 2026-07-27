package mocksf

import (
	"net/http/httptest"
	"testing"
)

// TestServer wraps an httptest.Server; .URL works exactly like a real
// httptest.Server since it's embedded.
type TestServer struct {
	*httptest.Server
	mock *Server
}

func (ts *TestServer) CallCount() int { return ts.mock.CallCount() }

// Start spins up a mock in-process over real HTTP, closed automatically
// at test cleanup.
func Start(t *testing.T, opts ...Option) *TestServer {
	t.Helper()
	s := New(opts...)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return &TestServer{Server: hs, mock: s}
}

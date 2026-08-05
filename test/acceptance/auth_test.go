package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestUnauthenticatedIsNotAServiceFailure pins the distinction the error
// vocabulary depends on. Every credential problem the caller can fix is a
// 401; 503 is reserved for the attribute source being unreachable, which no
// code path reaches yet.
//
// Until the pre-submission audit all of these returned 503, which is wrong
// in two directions at once: a client reading 503 retries an unauthenticated
// request instead of re-authenticating, and any monitor paging on 5xx pages
// an on-call for routine unauthenticated traffic.
func TestUnauthenticatedIsNotAServiceFailure(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	gw := harness.Start(t, testDeps(t, sf))

	for _, c := range []struct {
		name, header string
	}{
		{"no header at all", ""},
		{"missing Bearer prefix", "some-token"},
		{"unparseable claims", "Bearer not-json"},
		{"issuer not registered", `Bearer {"iss":"https://unknown.example","sub":"u_1","groups":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := gw.POST("/v1/query", `{"sql":"SELECT id FROM sf.accounts"}`, harness.RawAuth(c.header))
			require.Equal(t, 401, res.Code, "a credential the caller can fix must not be a 5xx")
			require.NotNil(t, res.Body.Error)
			require.Equal(t, "UNAUTHENTICATED", res.Body.Error.Code)
			require.NotEmpty(t, res.Body.TraceID, "an auth failure still has to be traceable")
		})
	}
}

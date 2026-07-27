package identity_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/identity"
	testdata "github.com/pradhanbv/emathp/internal/identity/fixtures"
)

// TestTenantDerivedFromIssuerNotClaim is ADR-011: a token asserting
// tenant_id: t_evilcorp must resolve to t_acme, because tenant comes from
// the verified issuer and the claim is never read.
func TestTenantDerivedFromIssuerNotClaim(t *testing.T) {
	raw := `{"iss":"https://acme-corp.okta.example","sub":"u_8f31c2",
		"tenant_id":"t_evilcorp","groups":["8f3c-4d21"]}`

	p, err := identity.Resolve(raw, testdata.IssuerRegistry())

	require.NoError(t, err)
	require.Equal(t, "t_acme", p.Tenant)        // from iss
	require.NotEqual(t, "t_evilcorp", p.Tenant) // claim ignored
	require.Equal(t, []string{"support_agent"}, p.Roles)
}

func TestResolveUnregisteredIssuerFailsClosed(t *testing.T) {
	raw := `{"iss":"https://unknown.example","sub":"u_1","groups":[]}`

	_, err := identity.Resolve(raw, testdata.IssuerRegistry())

	require.ErrorIs(t, err, identity.ErrPrincipalUnresolved)
}

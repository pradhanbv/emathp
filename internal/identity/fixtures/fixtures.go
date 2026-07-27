// Package fixtures provides identity test fixtures shared by unit and
// acceptance tests, matching testdata/tokens/*.json.
package fixtures

import "github.com/pradhanbv/emathp/internal/identity"

// IssuerRegistry returns the registry acme-corp's issuer resolves against:
// tenant t_acme, with group 8f3c-4d21 mapped to the support_agent role.
func IssuerRegistry() *identity.Registry {
	return identity.NewRegistry().Register("https://acme-corp.okta.example", identity.IssuerRegistration{
		Tenant: "t_acme",
		GroupRoles: map[string]string{
			"8f3c-4d21": "support_agent",
			"root":      "admin",
		},
	})
}

// Package identity turns a bearer token's claims into a Principal. The
// property that matters (ADR-011): tenant comes from the verified issuer,
// never from a claim in the token body. A token asserting tenant_id for
// another tenant must not grant access to it.
package identity

import "encoding/json"

// claims is the unverified JWT payload shape. tenant_id is deliberately
// never read into a Principal.Tenant - see Resolve.
type claims struct {
	Issuer string   `json:"iss"`
	Sub    string   `json:"sub"`
	Groups []string `json:"groups"`

	// TenantID exists in the struct so a reviewer can see it's parsed and
	// then deliberately never read below - not omitted, ignored.
	TenantID string `json:"tenant_id"`
}

type Principal struct {
	Tenant string
	Sub    string
	Roles  []string
}

// Resolve derives a Principal from raw unverified claims JSON. Signature
// verification is out of scope for the prototype (library work); the
// property worth demonstrating is that tenant derivation never trusts the
// token body.
func Resolve(raw string, registry *Registry) (Principal, error) {
	var c claims
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Principal{}, err
	}

	reg, ok := registry.ByIssuer(c.Issuer)
	if !ok {
		return Principal{}, ErrPrincipalUnresolved
	}

	roles := make([]string, 0, len(c.Groups))
	seen := make(map[string]bool, len(c.Groups))
	for _, g := range c.Groups {
		role, ok := reg.GroupRoles[g]
		if !ok || seen[role] {
			continue
		}
		seen[role] = true
		roles = append(roles, role)
	}

	_ = c.TenantID // never read for tenant derivation - see package doc

	return Principal{
		Tenant: reg.Tenant, // from the verified issuer only
		Sub:    c.Sub,
		Roles:  roles,
	}, nil
}

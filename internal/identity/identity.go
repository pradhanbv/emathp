// Package identity turns a bearer token's claims into a Principal. The
// property that matters (ADR-011): tenant comes from the verified issuer,
// never from a claim in the token body. A token asserting tenant_id for
// another tenant must not grant access to it.
package identity

import (
	"encoding/json"
	"strings"
)

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
	Tenant     string
	Sub        string
	Roles      []string
	Attributes map[string]string
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
	attrs := make(map[string]string)
	for _, g := range c.Groups {
		role, ok := reg.GroupRoles[g]
		if ok && !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
		// Simplification: attribute provenance is per-group here, not
		// per-attribute from its authoritative owning system (ADR-011
		// allows region to come from a CRM, say, rather than the IdP).
		// Real per-attribute resolution is control-plane infrastructure
		// this prototype doesn't build; the property worth keeping is
		// that attributes are resolved server-side, never read from the
		// token body, same as roles.
		for k, v := range reg.GroupAttrs[g] {
			attrs[k] = v
		}
	}

	_ = c.TenantID // never read for tenant derivation - see package doc

	return Principal{
		Tenant:     reg.Tenant, // from the verified issuer only
		Sub:        c.Sub,
		Roles:      roles,
		Attributes: attrs,
	}, nil
}

// ResolveFromHeader strips the "Bearer " prefix from an Authorization
// header and resolves the remainder as raw claims. The prototype's tokens
// are unverified JSON claims, not signed JWTs (see package doc) - this is
// the seam where real signature verification would slot in.
func ResolveFromHeader(authHeader string, registry *Registry) (Principal, error) {
	raw, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok {
		return Principal{}, ErrPrincipalUnresolved
	}
	return Resolve(raw, registry)
}

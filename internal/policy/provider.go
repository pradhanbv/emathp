package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RLSRule is one row-level-security residual, scoped to a table.
type RLSRule struct {
	Table string `json:"table"`
	Expr  string `json:"expr"`
}

// CLSRule is one column-level masking rule. Consumed starting with the CLS
// cycle; parsed here because it's part of the same policy document.
type CLSRule struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Fn     string `json:"fn"`
}

type ObjectRules struct {
	Deny []string `json:"deny"`
}

// RolePolicy is one role's compiled policy document - the shape an OPA
// Compile API response would take for this role.
type RolePolicy struct {
	Role          string      `json:"role"`
	RLS           []RLSRule   `json:"rls"`
	CLS           []CLSRule   `json:"cls"`
	Objects       ObjectRules `json:"objects"`
	PolicyVersion int         `json:"policy_version"`
}

// Provider answers policy questions per role, loaded once from JSON
// fixtures. It stands in for the OPA Compile API partial-evaluation call.
type Provider struct {
	roles map[string]RolePolicy
}

// Load reads every *.json file in dir as a RolePolicy, keyed by its
// "role" field.
func Load(dir string) (*Provider, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("policy: read %s: %w", dir, err)
	}

	p := &Provider{roles: make(map[string]RolePolicy)}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("policy: read %s: %w", path, err)
		}
		var rp RolePolicy
		if err := json.Unmarshal(data, &rp); err != nil {
			return nil, fmt.Errorf("policy: parse %s: %w", path, err)
		}
		p.roles[rp.Role] = rp
	}
	return p, nil
}

// ResidualsFor returns the RLS residuals role's policy declares against
// table - the "unknowns become residual predicates" half of OPA partial
// evaluation.
func (p *Provider) ResidualsFor(role, table string) (Residuals, error) {
	rp, ok := p.roles[role]
	if !ok {
		return nil, fmt.Errorf("policy: unknown role %q", role)
	}

	var out Residuals
	for _, r := range rp.RLS {
		if r.Table == table {
			out = append(out, Residual{Table: r.Table, Expr: r.Expr})
		}
	}
	return out, nil
}

// Masks is the CLS half of a role's compiled policy: which columns get
// masked, and how.
type Masks []CLSRule

// MasksFor returns the CLS rules role's policy declares against table.
func (p *Provider) MasksFor(role, table string) (Masks, error) {
	rp, ok := p.roles[role]
	if !ok {
		return nil, fmt.Errorf("policy: unknown role %q", role)
	}

	var out Masks
	for _, m := range rp.CLS {
		if m.Table == table {
			out = append(out, m)
		}
	}
	return out, nil
}

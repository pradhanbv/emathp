// Package catalog holds the per-table capability model: which predicates a
// connector can accept, and whether it actually enforces them (ENFORCED)
// or merely claims to (ADVISORY). This is control-plane state, loaded once
// from JSON fixtures.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Enforcement string

const (
	Enforced Enforcement = "ENFORCED"
	Advisory Enforcement = "ADVISORY"
)

type PredicateCapability struct {
	Ops         []string    `json:"ops"`
	Enforcement Enforcement `json:"enforcement"`
}

func (c PredicateCapability) SupportsOp(op string) bool {
	for _, o := range c.Ops {
		if o == op {
			return true
		}
	}
	return false
}

type Table struct {
	Name          string                         `json:"table"`
	Predicates    map[string]PredicateCapability `json:"predicates"`
	Masking       string                         `json:"masking"`
	JoinKeyInList bool                           `json:"join_key_in_list"`
	MaxInList     int                            `json:"max_in_list"`
}

type Catalog struct {
	tables map[string]Table
}

// Load reads every *.json file in dir as a Table capability profile, keyed
// by its "table" field (e.g. "sf.accounts").
func Load(dir string) (*Catalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s: %w", dir, err)
	}

	c := &Catalog{tables: make(map[string]Table)}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("catalog: read %s: %w", path, err)
		}
		var t Table
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("catalog: parse %s: %w", path, err)
		}
		c.tables[t.Name] = t
	}
	return c, nil
}

func (c *Catalog) Table(name string) (Table, bool) {
	t, ok := c.tables[name]
	return t, ok
}

// Capability looks up how table.column may be filtered: which ops the
// connector accepts, and whether it actually enforces them.
func (c *Catalog) Capability(table, column string) (PredicateCapability, bool) {
	t, ok := c.tables[table]
	if !ok {
		return PredicateCapability{}, false
	}
	cap, ok := t.Predicates[column]
	return cap, ok
}

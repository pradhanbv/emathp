// Package catalog holds the per-table capability model: which predicates a
// connector can accept, and whether it actually enforces them (ENFORCED)
// or merely claims to (ADVISORY). This is control-plane state, loaded once
// from JSON fixtures.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type Enforcement string

const (
	Enforced Enforcement = "ENFORCED"
	Advisory Enforcement = "ADVISORY"
)

// Masking declares whether a connector can mask a column itself. For the
// SaaS-REST-API connector category this design targets, genuine
// source-side masking is the exception rather than the norm - see
// DESIGN.md ADR-002 - so v1 only implements local (gateway-side) masking
// and treats any value other than MaskingUnsupported as an error rather
// than a pushdown opportunity it doesn't know how to use.
type Masking string

const MaskingUnsupported Masking = "unsupported"

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
	Masking       Masking                        `json:"masking"`
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

// ShapeHash stands in for the real design's connector_capability_version
// (DESIGN.md ADR-003): a plan cache key component that must change
// whenever a capability change could invalidate cached pushdown verdicts.
// A real system versions the catalog explicitly (a monotonic counter
// bumped on publish); this is a content hash instead - self-updating,
// but it can't distinguish "capability changed" from "capability changed
// back", which a real version number can. Good enough for a prototype
// with no catalog-publish pipeline to version against.
func (c *Catalog) ShapeHash(table string) (string, error) {
	t, ok := c.tables[table]
	if !ok {
		return "", fmt.Errorf("catalog: unknown table %q", table)
	}

	cols := make([]string, 0, len(t.Predicates))
	for col := range t.Predicates {
		cols = append(cols, col)
	}
	sort.Strings(cols)

	h := sha256.New()
	for _, col := range cols {
		p := t.Predicates[col]
		fmt.Fprintf(h, "%s:%v:%s;", col, p.Ops, p.Enforcement)
	}
	fmt.Fprintf(h, "masking:%s;", t.Masking)

	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

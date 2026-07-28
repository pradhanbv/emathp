// Package plancache caches built plans behind a composite key, and is
// where a plan actually gets built or reused. Every component of Key
// exists to close one specific leak (DESIGN.md ADR-003) - the pair of
// tests this cycle carries proves both halves: unsafe key components
// would leak across roles, but the key still has to be loose enough
// (parameterized on value, not shape) to be worth having at all.
package plancache

import (
	"sort"
	"strings"
	"sync"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/plan"
	"github.com/pradhanbv/emathp/internal/policy"
)

// Key is the plan cache key. Every field is load-bearing:
//   - SQLShape lets different literal values (status='open' vs 'closed')
//     share a cached plan - the only field allowed to cause a hit across
//     otherwise-different queries.
//   - Tenant, RoleSet: a plan built under one tenant or role must never
//     be served to another - the privilege-escalation vector this cycle
//     exists to close.
//   - PolicyVersion, PolicyShape: a policy rewrite (new rule, or just a
//     new version stamp) must invalidate plans built under the old one.
//   - CapShape: a capability change alters which predicates are
//     ENFORCED, invalidating cached pushdown verdicts.
type Key struct {
	SQLShape      string
	Tenant        string
	PolicyVersion int
	PolicyShape   string
	CapShape      string
	RoleSet       string
}

// Cache is an in-memory plan cache, safe for concurrent use. It has no
// eviction policy (real design: per-pod LRU backed by a shared Redis
// tier, ADR-003) - a fine gap for a prototype whose fixtures are a
// handful of plans, not a real workload's cardinality.
type Cache struct {
	mu    sync.Mutex
	plans map[Key]*plan.Plan
}

func New() *Cache {
	return &Cache{plans: make(map[Key]*plan.Plan)}
}

// Reset is test-only: a Cache is meant to live for a gateway process's
// whole lifetime, but tests need one starting empty per test case.
func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans = make(map[Key]*plan.Plan)
}

func (c *Cache) get(key Key) (*plan.Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.plans[key]
	return p, ok
}

func (c *Cache) put(key Key, p *plan.Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plans[key] = p
}

// Resolve returns the plan for (tenant, role, sql): from cache if key
// matches, freshly built (and cached) otherwise. The returned plan is
// never mutated in place - see exec.Run's doc comment on why binding
// happens per call, not by writing into a shared cached plan.
func Resolve(cache *Cache, sql string, cat *catalog.Catalog, pol *policy.Provider, tenant, role string) (*plan.Plan, Key, bool, error) {
	tables, err := plan.ParseTables(sql)
	if err != nil {
		return nil, Key{}, false, err
	}

	shape, err := plan.Shape(sql)
	if err != nil {
		return nil, Key{}, false, err
	}
	policyVersion, err := pol.Version(role)
	if err != nil {
		return nil, Key{}, false, err
	}
	policyShape, err := pol.ShapeHash(role)
	if err != nil {
		return nil, Key{}, false, err
	}
	capShape, err := combinedCapShape(cat, tables)
	if err != nil {
		return nil, Key{}, false, err
	}

	key := Key{
		SQLShape:      shape,
		Tenant:        tenant,
		PolicyVersion: policyVersion,
		PolicyShape:   policyShape,
		CapShape:      capShape,
		RoleSet:       role,
	}

	if p, ok := cache.get(key); ok {
		return p, key, true, nil
	}

	var residuals policy.Residuals
	var masks policy.Masks
	for _, table := range tables {
		r, err := pol.ResidualsFor(role, table)
		if err != nil {
			return nil, key, false, err
		}
		residuals = append(residuals, r...)
		m, err := pol.MasksFor(role, table)
		if err != nil {
			return nil, key, false, err
		}
		masks = append(masks, m...)
	}

	p, err := plan.Build(sql, cat, residuals, masks)
	if err != nil {
		return nil, key, false, err
	}

	cache.put(key, p)
	return p, key, false, nil
}

// combinedCapShape folds every referenced table's capability shape into
// one cache-key component - a join's cached plan must invalidate if
// either side's capability profile changes, not just one.
func combinedCapShape(cat *catalog.Catalog, tables []string) (string, error) {
	shapes := make([]string, 0, len(tables))
	for _, t := range tables {
		h, err := cat.ShapeHash(t)
		if err != nil {
			return "", err
		}
		shapes = append(shapes, t+":"+h)
	}
	sort.Strings(shapes)
	return strings.Join(shapes, "|"), nil
}

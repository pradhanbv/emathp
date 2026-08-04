package plancache_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/plancache"
	"github.com/pradhanbv/emathp/internal/policy"
	"github.com/pradhanbv/emathp/internal/ratelimit"
)

// TestConcurrentResolvesDoNotBleedAcrossRoles is the privilege-escalation
// test under contention. TestPlanCacheDoesNotLeakAcrossRoles proves admin
// and support get separate entries when the two resolves are serial; this
// proves the same holds when they race for one cache, which is how the
// gateway actually runs. A cache that published a partially-built entry,
// or keyed on anything narrower than the full Key, would surface here as
// a support principal receiving an unmasked plan - the exact leak the key
// exists to prevent.
func TestConcurrentResolvesDoNotBleedAcrossRoles(t *testing.T) {
	cat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)
	pol, err := policy.Load("../../testdata/policy")
	require.NoError(t, err)

	cache := plancache.New()
	const sql = "SELECT id,email FROM sf.accounts"

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			role := "admin"
			if i%2 == 1 {
				role = "support_agent"
			}
			p, _, _, err := plancache.Resolve(cache, sql, cat, pol, "t_acme", role)
			if err != nil {
				errs <- err
				return
			}
			masked := false
			for _, c := range p.OutputColumns() {
				if c.Name == "email" && c.Mask != nil {
					masked = true
				}
			}
			if role == "support_agent" && !masked {
				errs <- errUnmaskedForSupport
			}
			if role == "admin" && masked {
				errs <- errMaskedForAdmin
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// TestConcurrentTenantsDoNotShareAPlan closes the one Key field with no
// test at all. Every other plan-cache test hardcodes "t_acme", so role
// separation is proven and tenant separation - the more severe of the two
// leaks in a multi-tenant product - was only ever assumed.
func TestConcurrentTenantsDoNotShareAPlan(t *testing.T) {
	cat, err := catalog.Load("../../testdata/catalog")
	require.NoError(t, err)
	pol, err := policy.Load("../../testdata/policy")
	require.NoError(t, err)

	cache := plancache.New()
	const sql = "SELECT id,email FROM sf.accounts"

	_, keyA, _, err := plancache.Resolve(cache, sql, cat, pol, "t_acme", "support_agent")
	require.NoError(t, err)
	_, keyB, hitB, err := plancache.Resolve(cache, sql, cat, pol, "t_globex", "support_agent")
	require.NoError(t, err)

	require.NotEqual(t, keyA, keyB, "two tenants must never share a plan cache key")
	require.False(t, hitB, "t_globex must not receive t_acme's cached plan")
}

// TestLimiterAdmitsExactlyTheBudgetUnderConcurrency guards the property
// ADR-006 depends on: a budget of N admits exactly N, no matter how many
// callers race for it. Allow() takes the mutex around its own
// check-and-decrement, so this should hold - it is a regression guard on
// a classic TOCTOU, not a suspected bug.
func TestLimiterAdmitsExactlyTheBudgetUnderConcurrency(t *testing.T) {
	const budget, callers = 25, 200
	lim := ratelimit.New()
	lim.SetLimit("sf", budget)

	var admitted, wg = make(chan struct{}, callers), sync.WaitGroup{}
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lim.Allow("sf") {
				admitted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(admitted)
	require.Len(t, admitted, budget, "a budget of %d must admit exactly %d under any interleaving", budget, budget)
}

var (
	errUnmaskedForSupport = errPlan("support_agent received an UNMASKED plan - admin's entry leaked")
	errMaskedForAdmin     = errPlan("admin received a MASKED plan - support's entry leaked")
)

type errPlan string

func (e errPlan) Error() string { return string(e) }

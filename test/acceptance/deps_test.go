package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/identity/fixtures"
	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/plancache"
	"github.com/pradhanbv/emathp/internal/policy"
	"github.com/pradhanbv/emathp/internal/server"
)

const defaultCatalogDir = "../../testdata/catalog"

// testDeps builds real gateway dependencies - the same catalog and policy
// fixtures every other cycle's tests use - wired to sf as the "sf"
// connector. This is what makes these acceptance tests, not more unit
// tests: everything downstream of the HTTP boundary is real.
func testDeps(t *testing.T, sf *mocksf.TestServer) server.Deps {
	t.Helper()
	return testDepsWithCatalog(t, sf, defaultCatalogDir)
}

// testDepsWithCatalog is for tests that need a capability profile the
// shared fixture doesn't have - e.g. region ENFORCED rather than
// ADVISORY, to exercise the verification filter.
func testDepsWithCatalog(t *testing.T, sf *mocksf.TestServer, catalogDir string) server.Deps {
	t.Helper()

	cat, err := catalog.Load(catalogDir)
	require.NoError(t, err)

	pol, err := policy.Load("../../testdata/policy")
	require.NoError(t, err)

	return server.Deps{
		Catalog:   cat,
		Policy:    pol,
		Identity:  fixtures.IssuerRegistry(),
		PlanCache: plancache.New(),
		Sources: map[string]connector.Source{
			"sf": connector.NewHTTPSource(sf.URL),
		},
	}
}

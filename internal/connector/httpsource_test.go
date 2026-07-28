package connector_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/catalog"
	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/mocksf"
)

// TestConnectorPaginationAndETag proves the connector SDK's two load-
// bearing mechanics: pagination (many small pages add up to the full row
// set, with the call count to prove it wasn't one lucky big page) and
// ETag/If-None-Match -> 304. The ETag half is proven directly against the
// mock rather than through connector.Source, since Source doesn't carry a
// conditional-request parameter yet - that lands with freshness (Cycle 9),
// the first thing that actually needs it.
func TestConnectorPaginationAndETag(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(250), mocksf.PageSize(100))
	source := connector.NewHTTPSource(sf.URL)

	rows, _, err := source.Fetch(context.Background(), connector.FetchRequest{
		Table:   "sf.accounts",
		Columns: []string{"id"},
	})
	require.NoError(t, err)
	require.Len(t, rows, 250)
	require.Equal(t, 3, sf.CallCount(), "250 rows at page size 100 is 3 calls, not one big page")

	resp, err := http.Get(sf.URL + "/accounts")
	require.NoError(t, err)
	defer resp.Body.Close()
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)

	req, err := http.NewRequest(http.MethodGet, sf.URL+"/accounts", nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusNotModified, resp2.StatusCode)
}

// TestHideColumnFailsTheCall proves the field-level-security simulation
// the exec fail-closed test (Cycle 4) needs a real connector to exercise
// later: requesting a hidden column surfaces as ColumnUnavailableError,
// not a silently-omitted field.
func TestHideColumnFailsTheCall(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5), mocksf.HideColumn("region"))
	source := connector.NewHTTPSource(sf.URL)

	_, _, err := source.Fetch(context.Background(), connector.FetchRequest{
		Table:   "sf.accounts",
		Columns: []string{"id", "region"},
	})

	var colErr *connector.ColumnUnavailableError
	require.ErrorAs(t, err, &colErr)
	require.Equal(t, "region", colErr.Column)
}

// TestCapabilityFilteringAndLieAbout proves the mock's two-option
// combination Cycle 6 depends on: Capability alone actually filters
// server-side (honest), and pairing it with LieAbout cancels that back
// out (declares enforcement, doesn't apply it) without changing the
// declared capability itself.
func TestCapabilityFilteringAndLieAbout(t *testing.T) {
	honest := mocksf.Start(t, mocksf.Rows(10), mocksf.Capability("region", catalog.Enforced))
	honestSource := connector.NewHTTPSource(honest.URL)
	rows, _, err := honestSource.Fetch(context.Background(), connector.FetchRequest{
		Table:   "sf.accounts",
		Columns: []string{"id", "region"},
		Filters: map[string][]string{"region": {"EMEA"}},
	})
	require.NoError(t, err)
	for _, r := range rows {
		require.Equal(t, "EMEA", r["region"])
	}
	require.NotEmpty(t, rows)
	require.Less(t, len(rows), 10, "only half the fixture rows are EMEA")

	lying := mocksf.Start(t, mocksf.Rows(10), mocksf.Capability("region", catalog.Enforced), mocksf.LieAbout("region"))
	lyingSource := connector.NewHTTPSource(lying.URL)
	rows, _, err = lyingSource.Fetch(context.Background(), connector.FetchRequest{
		Table:   "sf.accounts",
		Columns: []string{"id", "region"},
		Filters: map[string][]string{"region": {"EMEA"}},
	})
	require.NoError(t, err)
	require.Len(t, rows, 10, "lying connector ignores the filter it claims to enforce")
}

// TestFetchWithETagReturnsNotModified proves the connector.Source-level
// conditional-fetch path the freshness cache (Cycle 9) depends on: passing
// back the ETag a prior Fetch returned short-circuits to NotModified with
// zero rows, rather than re-fetching data the caller already has.
func TestFetchWithETagReturnsNotModified(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Rows(5))
	source := connector.NewHTTPSource(sf.URL)

	_, meta, err := source.Fetch(context.Background(), connector.FetchRequest{
		Table:   "sf.accounts",
		Columns: []string{"id"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, meta.ETag)
	require.False(t, meta.NotModified)

	rows, meta2, err := source.Fetch(context.Background(), connector.FetchRequest{
		Table:   "sf.accounts",
		Columns: []string{"id"},
		ETag:    meta.ETag,
	})
	require.NoError(t, err)
	require.True(t, meta2.NotModified)
	require.Empty(t, rows)
}

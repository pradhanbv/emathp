package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/mockzd"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestSemiJoinReducesProbeCalls is the KEY test for Cycle 10 (ADR-007):
// proves the semi-join rewrite actually reduces connector calls, not just
// that a cross-connector join can be answered at all. sf.accounts (500
// rows, the build side) is fully scanned once; zd.tickets (50,000 rows,
// the probe side) would need one call per build row if unbatched - instead
// its organization_id join key is pushed as chunked IN-lists capped by
// mockzd.MaxInList, so the probe side costs a small, bounded number of
// calls regardless of the build side's size.
func TestSemiJoinReducesProbeCalls(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Accounts(500, "EMEA"))
	zd := mockzd.Start(t, mockzd.Tickets(50_000, "open"), mockzd.MaxInList(200))
	gw := harness.Start(t, testDepsJoin(t, sf, zd))

	res := gw.Query("admin", `
		SELECT a.name, t.subject FROM sf.accounts a
		JOIN zd.tickets t ON t.organization_id = a.external_id
		WHERE t.status = 'open'`)

	require.Equal(t, 200, res.Code)
	require.LessOrEqual(t, zd.CallCount(), 20, "semi-join: a few chunked calls, not one per build row")
	require.NotNil(t, res.Body.Meta)
	require.Equal(t, "semi_join", res.Body.Meta.JoinStrategy)
	require.Greater(t, res.Body.Meta.NaiveCallEstimate, 400)
	require.NotEmpty(t, res.Body.Rows, "the join should actually match rows, not just avoid calls")
	require.Equal(t, []string{"name", "subject"}, res.Body.Columns)
}

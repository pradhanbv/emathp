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

// TestJoinKeepsBothSidesOfCollidingColumns pins the correctness half of the
// join that TestSemiJoinReducesProbeCalls (above) does not: that one asserts
// call *count* and only NotEmpty on the rows, so a join returning the wrong
// values passes it. Both mocks expose an "id" column, and hashJoin merges
// build then probe into one flat row - so before this test, `a.id` silently
// returned the ticket's id. The alias is known at plan time
// (projectionQualified requires it) and was being discarded at ProjectCol
// construction; ProjectCol.Side carries it through to the merge, and rows
// are positional so two columns named "id" are two slots rather than one
// overwritten map entry.
//
// This also fixes over-projection's collision surface: OPA residuals and
// masks add columns to each side's scan, widening the set of names that can
// clash. Those extra columns are consumed per-side before the merge
// (fetchScanRows applies residuals and the verification filter), so security
// was never at risk - but a mask applied post-merge would have masked the
// other side's value.
func TestJoinKeepsBothSidesOfCollidingColumns(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Accounts(5, "EMEA"))
	zd := mockzd.Start(t, mockzd.Tickets(20, "open"), mockzd.MaxInList(200))
	gw := harness.Start(t, testDepsJoin(t, sf, zd))

	res := gw.Query("admin", `
		SELECT a.id, t.id FROM sf.accounts a
		JOIN zd.tickets t ON t.organization_id = a.external_id
		WHERE t.status = 'open'`)

	require.Equal(t, 200, res.Code)
	require.NotEmpty(t, res.Body.Rows)
	// Both columns are reported as "id" - the same answer Postgres gives -
	// because rows are positional, so two columns of one name are simply
	// two slots. Position, not the key, carries which side each came from.
	require.Equal(t, []string{"id", "id"}, res.Body.Columns)
	require.Equal(t, "a000000", res.Body.Rows[0][0], "a.id must be the ACCOUNT's id, not the ticket's")
	require.Equal(t, "t000000", res.Body.Rows[0][1], "t.id must be the ticket's id")
}

// TestSemiJoinReturnsEveryMatchingRow pins cardinality, which the call-count
// test leaves open: 500 accounts (ext-000000..ext-000499) against 50,000
// tickets whose organization_id cycles a 10,000-value space gives 5 tickets
// per account, so the join is exactly 2,500 rows. NotEmpty would pass on 1.
// It also spans a chunk boundary - 500 keys at MaxInList 200 is 3 chunks -
// so an off-by-one in chunkStrings drops rows here rather than silently.
func TestSemiJoinReturnsEveryMatchingRow(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Accounts(500, "EMEA"))
	zd := mockzd.Start(t, mockzd.Tickets(50_000, "open"), mockzd.MaxInList(200))
	gw := harness.Start(t, testDepsJoin(t, sf, zd))

	res := gw.Query("admin", `
		SELECT a.name, t.subject FROM sf.accounts a
		JOIN zd.tickets t ON t.organization_id = a.external_id
		WHERE t.status = 'open'`)

	require.Equal(t, 200, res.Code)
	require.Len(t, res.Body.Rows, 2500, "500 accounts x 5 tickets each, across 3 IN-list chunks")
}

// TestJoinDoesNotLeakOverProjectedColumnAcrossSides is the sharper half of
// the collision problem. Over-projection (plan.buildSideTree) adds every
// predicate column - pushed or locally-filtered - plus mask and join-key
// columns to a side's scan, so a side fetches columns nothing selected. Here
// t.status is only a WHERE predicate, never projected, but it is fetched;
// a.status IS selected. Both are named "status", so a flat merge let the
// probe's internal predicate column overwrite the build's selected value and
// `a.status` returned the ticket's "pending" instead of the account's "open".
//
// This is the case OPA widens: residuals are predicates, so every RLS rule
// adds another over-projected column and another chance of a same-name clash
// with the other side. The filter phase itself is safe either way -
// fetchScanRows applies residuals, the verification filter, and local filters
// per side, all strictly before hashJoin - so this was never an entitlement
// bypass. It was the output being drawn from the wrong side.
func TestJoinDoesNotLeakOverProjectedColumnAcrossSides(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Accounts(5, "EMEA"))              // status "open"
	zd := mockzd.Start(t, mockzd.Tickets(20, "pending"), mockzd.MaxInList(200))
	gw := harness.Start(t, testDepsJoin(t, sf, zd))

	res := gw.Query("admin", `
		SELECT a.status, t.subject FROM sf.accounts a
		JOIN zd.tickets t ON t.organization_id = a.external_id
		WHERE t.status = 'pending'`)

	require.Equal(t, 200, res.Code)
	require.NotEmpty(t, res.Body.Rows)
	require.Equal(t, "open", res.Body.Rows[0][0],
		"a.status is the account's own value, not the probe side's over-projected predicate column")
}

// TestOuterJoinsRejectedNotSilentlyDowngraded: LEFT/RIGHT/NATURAL JOIN parse
// into the same *sqlparser.JoinTableExpr as INNER, so before this check they
// ran through the same hand-rolled hash join and returned inner-join results
// under a 200 - a LEFT JOIN silently losing every unmatched left row, which
// is the one thing the caller asked to keep. v1's executor is a stand-in for
// ADR-007 tier 1 (in-process DuckDB); rejecting is the honest boundary until
// a real SQL engine runs the join, and it matches how the SQL surface already
// treats cross-source disjunctions (UNSUPPORTED_PREDICATE rather than a
// silent full scan).
func TestOuterJoinsRejectedNotSilentlyDowngraded(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Accounts(5, "EMEA"))
	zd := mockzd.Start(t, mockzd.Tickets(20, "open"), mockzd.MaxInList(200))
	gw := harness.Start(t, testDepsJoin(t, sf, zd))

	for _, join := range []string{"LEFT JOIN", "RIGHT JOIN", "NATURAL JOIN"} {
		res := gw.Query("admin", `
			SELECT a.name, t.subject FROM sf.accounts a `+join+`
			zd.tickets t ON t.organization_id = a.external_id
			WHERE t.status = 'open'`)
		require.Equal(t, 400, res.Code, join+" must be rejected, not run as an inner join")
	}

	// The supported form still works.
	res := gw.Query("admin", `
		SELECT a.name, t.subject FROM sf.accounts a JOIN zd.tickets t
		ON t.organization_id = a.external_id WHERE t.status = 'open'`)
	require.Equal(t, 200, res.Code)
	require.NotEmpty(t, res.Body.Rows)
}

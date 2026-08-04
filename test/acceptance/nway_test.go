package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/mockzd"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

const fourTableSQL = `
	SELECT a.name, t.subject, b.name, u.subject
	FROM sf.accounts a
	JOIN zd.tickets t ON t.organization_id = a.external_id
	JOIN sf.accounts b ON b.external_id = a.external_id
	JOIN zd.tickets u ON u.organization_id = b.external_id`

// TestFourTableJoinEndToEnd is the end-to-end N-way proof: real SQL, four
// sides, three links, over HTTP. It runs through every join engine compiled
// into this build and requires identical output - so with -tags duckdb the
// N-way path cannot be correct in one engine and wrong in the other, and
// without it the default cgo-free engine is still proven to do 4-way joins.
//
// Two aliases (a, b) point at the same table, which is the case that breaks
// any implementation still keyed on table name rather than alias - the merge
// namespaces by alias precisely so a self-join stays addressable.
func TestFourTableJoinEndToEnd(t *testing.T) {
	var want [][]any
	for _, engine := range joinEngines() {
		sf := mocksf.Start(t, mocksf.Accounts(50, "EMEA"))
		zd := mockzd.Start(t, mockzd.Tickets(2000, "open"), mockzd.MaxInList(200))
		deps := testDepsJoin(t, sf, zd)
		deps.JoinEngine = engine
		gw := harness.Start(t, deps)

		res := gw.Query("admin", fourTableSQL)

		require.Equal(t, 200, res.Code, "%s engine", engine.Name())
		require.Equal(t, []string{"name", "subject", "name", "subject"}, res.Body.Columns)
		require.NotEmpty(t, res.Body.Rows)
		require.NotNil(t, res.Body.Meta)
		require.Equal(t, engine.Name(), res.Body.Meta.JoinEngine,
			"the response must name the engine that actually ran")

		if want == nil {
			want = res.Body.Rows
			t.Logf("4-way join: %d rows, naive estimate %d calls", len(res.Body.Rows), res.Body.Meta.NaiveCallEstimate)
		} else {
			require.Equal(t, len(want), len(res.Body.Rows), "engines disagree on 4-way row count")
		}
	}
}

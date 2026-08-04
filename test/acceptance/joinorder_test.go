package acceptance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pradhanbv/emathp/internal/mocksf"
	"github.com/pradhanbv/emathp/internal/mockzd"
	"github.com/pradhanbv/emathp/test/acceptance/harness"
)

// TestJoinRejectsForwardReference pins the constraint that makes the cascade
// possible: every join must reference a table appearing earlier in FROM, so
// the IN-list pushed into a probe is built from rows already fetched. A
// forward reference is refused rather than silently reordered.
func TestJoinRejectsForwardReference(t *testing.T) {
	sf := mocksf.Start(t, mocksf.Accounts(5, "EMEA"))
	zd := mockzd.Start(t, mockzd.Tickets(20, "open"), mockzd.MaxInList(200))
	gw := harness.Start(t, testDepsJoin(t, sf, zd))

	res := gw.Query("admin", `
		SELECT a.name, t.subject
		FROM sf.accounts a
		JOIN zd.tickets t ON t.organization_id = b.external_id
		JOIN sf.accounts b ON b.external_id = a.external_id`)

	require.NotEqual(t, 200, res.Code)
	require.NotNil(t, res.Body.Error)
	t.Logf("forward reference rejected: %s", res.Body.Error.Message)
}

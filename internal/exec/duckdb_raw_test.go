//go:build duckdb

package exec

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/marcboeker/go-duckdb"

	"github.com/pradhanbv/emathp/internal/connector"
)

// TestDuckDBIsNWayCapable is the engine-level half of the N-way proof: four
// tables joined in one query, one instance, one pass. The executor-level
// half - that runJoin's semi-join cascade and both JoinEngines handle four
// sides - is TestEnginesAgreeOnFourWayJoin below.
func TestDuckDBIsNWayCapable(t *testing.T) {
	a, b, c, d := fourSides()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for name, rows := range map[string][]connector.Row{"a": a, "b": b, "c": c, "d": d} {
		if err := loadSide(ctx, conn, name, columnsOf(rows), rows); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	q := `SELECT count(*) FROM a
	      JOIN b ON b.k1 = a.k1
	      JOIN c ON c.k2 = b.k2
	      JOIN d ON d.k3 = c.k3`
	if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("engine rejected a 4-way join: %v", err)
	}
	if n == 0 {
		t.Fatal("expected a non-empty 4-way result")
	}
	t.Logf("4-way join in one query: %d rows - the engine has no 2-table limit", n)
}

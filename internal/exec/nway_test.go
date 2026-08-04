package exec

import (
	"context"
	"fmt"
	"testing"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/plan"
)

// Four tables chained a -> b -> c -> d, each link on a different key, so a
// wrong join order or a dropped link shows up as a wrong row count rather
// than an error.
func fourSides() (a, b, c, d []connector.Row) {
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("k%d", i%5)
		a = append(a, connector.Row{"id": fmt.Sprintf("a%d", i), "k1": k})
		b = append(b, connector.Row{"id": fmt.Sprintf("b%d", i), "k1": k, "k2": k})
		c = append(c, connector.Row{"id": fmt.Sprintf("c%d", i), "k2": k, "k3": k})
		d = append(d, connector.Row{"id": fmt.Sprintf("d%d", i), "k3": k})
	}
	return
}

// TestEnginesAgreeOnFourWayJoin is the executor-level N-way proof: the same
// four sides through both engines, via the interface runJoin actually calls.
// Chained aliases matter here - the merge namespaces by alias, so a middle
// table's key has to survive being both a probe (for the link reaching it)
// and a build side (for the link leaving it).
func TestEnginesAgreeOnFourWayJoin(t *testing.T) {
	a, b, c, d := fourSides()
	sides := []JoinInput{
		{Alias: "a", Rows: a}, {Alias: "b", Rows: b},
		{Alias: "c", Rows: c}, {Alias: "d", Rows: d},
	}
	links := []plan.Link{
		{From: 0, FromCol: "k1", To: 1, ToCol: "k1"},
		{From: 1, FromCol: "k2", To: 2, ToCol: "k2"},
		{From: 2, FromCol: "k3", To: 3, ToCol: "k3"},
	}

	var want []string
	for _, e := range engines() {
		got, err := e.Join(context.Background(), sides, links)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		// every side must still be addressable under its own alias
		for _, al := range []string{"a", "b", "c", "d"} {
			if _, ok := got[0][sideKey(al, "id")]; !ok {
				t.Fatalf("%s: alias %q lost its id column in the merge", e.Name(), al)
			}
		}
		n := normalize(got)
		if want == nil {
			want = n
			t.Logf("4-way through the engine interface: %d rows", len(got))
		} else if len(want) != len(n) {
			t.Fatalf("engines disagree on 4-way: go=%d rows, %s=%d rows", len(want), e.Name(), len(n))
		}
	}
}

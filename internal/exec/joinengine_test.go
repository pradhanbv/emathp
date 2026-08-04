package exec

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/pradhanbv/emathp/internal/connector"
	"github.com/pradhanbv/emathp/internal/plan"
)

// sidesOf/linksOf keep these tests reading as two-table cases against the
// N-way interface - a 2-table join is just N=2.
func sidesOf(build, probe []connector.Row) []JoinInput {
	return []JoinInput{{Alias: "a", Rows: build}, {Alias: "t", Rows: probe}}
}

func linksOf(on plan.Equi) []plan.Link {
	return []plan.Link{{From: 0, FromCol: on.LeftCol, To: 1, ToCol: on.RightCol}}
}

func normalize(rows []connector.Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := ""
		for _, k := range keys {
			s += k + "=" + r[k] + ";"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// TestEnginesAgreeOnCollidingColumns is the property that actually matters:
// both sides expose "id", and the merge has to keep both. A flat merge
// resolves a.id to whichever side was written last - the bug
// TestJoinKeepsBothSidesOfCollidingColumns pins at the HTTP layer. Here it is
// a contract every JoinEngine must satisfy, so a new engine cannot
// reintroduce it.
func TestEnginesAgreeOnCollidingColumns(t *testing.T) {
	build := []connector.Row{
		{"id": "a1", "external_id": "x", "name": "Acme"},
		{"id": "a2", "external_id": "y", "name": "Beta"},
	}
	probe := []connector.Row{
		{"id": "t1", "organization_id": "x", "subject": "down"},
		{"id": "t2", "organization_id": "x", "subject": "slow"},
		{"id": "t3", "organization_id": "z", "subject": "orphan"}, // matches nothing
	}
	on := plan.Equi{LeftCol: "external_id", RightCol: "organization_id"}

	var want []string
	for _, e := range engines() {
		got, err := e.Join(context.Background(), sidesOf(build, probe), linksOf(on))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if len(got) != 2 {
			t.Fatalf("%s: want 2 joined rows, got %d", e.Name(), len(got))
		}
		for _, r := range got {
			if r[sideKey("a", "id")] == r[sideKey("t", "id")] {
				t.Fatalf("%s: sides collapsed onto one id - both are %q", e.Name(), r[sideKey("a", "id")])
			}
		}
		n := normalize(got)
		if want == nil {
			want = n
		} else if !reflect.DeepEqual(want, n) {
			t.Fatalf("engines disagree:\n  first  = %v\n  %s = %v", want, e.Name(), n)
		}
	}
}

// TestEnginesAgreeOnFanout covers the many-to-many case, where one build key
// matches several probe rows and vice versa - the shape where an engine that
// deduplicates or short-circuits would silently drop rows.
func TestEnginesAgreeOnFanout(t *testing.T) {
	var build, probe []connector.Row
	for i := 0; i < 50; i++ {
		build = append(build, connector.Row{"id": "b" + string(rune('a'+i%26)), "k": "k" + string(rune('a'+i%5))})
	}
	for i := 0; i < 200; i++ {
		probe = append(probe, connector.Row{"id": "p" + string(rune('a'+i%26)), "k": "k" + string(rune('a'+i%5))})
	}
	on := plan.Equi{LeftCol: "k", RightCol: "k"}

	var want []string
	for _, e := range engines() {
		got, err := e.Join(context.Background(), sidesOf(build, probe), linksOf(on))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		n := normalize(got)
		if want == nil {
			want = n
			t.Logf("fanout produced %d rows", len(got))
		} else if !reflect.DeepEqual(want, n) {
			t.Fatalf("engines disagree on fanout: first=%d rows, %s=%d rows", len(want), e.Name(), len(n))
		}
	}
}

// TestEnginesAgreeOnEmptySide pins inner-join semantics at the boundary: an
// empty side means no output, not an error and not the other side passed
// through.
func TestEnginesAgreeOnEmptySide(t *testing.T) {
	rows := []connector.Row{{"id": "a", "k": "x"}}
	on := plan.Equi{LeftCol: "k", RightCol: "k"}
	for _, e := range engines() {
		for _, c := range []struct {
			name         string
			build, probe []connector.Row
		}{
			{"empty build", nil, rows},
			{"empty probe", rows, nil},
			{"both empty", nil, nil},
		} {
			got, err := e.Join(context.Background(), sidesOf(c.build, c.probe), linksOf(on))
			if err != nil {
				t.Fatalf("%s/%s: %v", e.Name(), c.name, err)
			}
			if len(got) != 0 {
				t.Fatalf("%s/%s: want 0 rows, got %d", e.Name(), c.name, len(got))
			}
		}
	}
}

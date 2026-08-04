package exec

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/pradhanbv/emathp/internal/connector"
)

// TestProbeChunksAreIndependentOfBuildRowOrder pins the property that
// makes probe-side result caching work at all.
//
// Each chunk of join keys becomes part of a probe-side cache key, because
// freshness.cacheKey folds the bound filter values in. That key sorts
// values *within* a chunk - which canonicalizes how one chunk is written,
// but says nothing about which keys land in which chunk. Chunk membership
// used to follow the build side's row order, and a SaaS list endpoint
// guarantees no ordering without an explicit sort. So the same probe data,
// reached through a build side that came back in a different order,
// produced a completely different set of cache keys and missed every
// probe-side entry - on the larger side of the join, since the semi-join
// fetches the smaller side as build.
func TestProbeChunksAreIndependentOfBuildRowOrder(t *testing.T) {
	const keyCol, chunkSize = "external_id", 7

	ordered := make([]connector.Row, 0, 40)
	for i := 0; i < 40; i++ {
		ordered = append(ordered, connector.Row{keyCol: string(rune('a'+i%26)) + string(rune('0'+i/26))})
	}

	shuffled := append([]connector.Row(nil), ordered...)
	rand.New(rand.NewSource(7)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	if reflect.DeepEqual(ordered, shuffled) {
		t.Fatal("shuffle was a no-op; the test would prove nothing")
	}

	a := chunkStrings(distinctValues(ordered, keyCol), chunkSize)
	b := chunkStrings(distinctValues(shuffled, keyCol), chunkSize)

	if !reflect.DeepEqual(a, b) {
		t.Fatalf("chunk membership follows build-side row order, so every probe-side\n"+
			"cache key changes when the source reorders identical rows:\n  ordered:  %v\n  shuffled: %v", a, b)
	}
}

// TestDistinctValuesStillDedupesAndDropsEmpty guards the behaviour sorting
// had to preserve: duplicates collapse, empty join keys are skipped
// entirely rather than pushed to the probe side as an empty IN-list value.
func TestDistinctValuesStillDedupesAndDropsEmpty(t *testing.T) {
	got := distinctValues([]connector.Row{
		{"k": "b"}, {"k": ""}, {"k": "a"}, {"k": "b"}, {"k": "c"}, {"k": ""},
	}, "k")
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

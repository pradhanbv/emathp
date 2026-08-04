package freshness

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pradhanbv/emathp/internal/connector"
)

// countingSource answers every fetch as 304 Not Modified, the response
// that drives the revalidation path - the one that used to mutate a
// shared *entry in place.
type countingSource struct {
	calls       atomic.Int64
	notModified bool
	blockUntil  chan struct{}
}

func (s *countingSource) Fetch(_ context.Context, _ connector.FetchRequest) ([]connector.Row, connector.FetchMeta, error) {
	s.calls.Add(1)
	if s.blockUntil != nil {
		<-s.blockUntil
	}
	if s.notModified {
		return nil, connector.FetchMeta{NotModified: true}, nil
	}
	return []connector.Row{{"id": "1"}}, connector.FetchMeta{ETag: "v1"}, nil
}

func srcFor(c *Cache, inner connector.Source, staleness time.Duration) *Source {
	return &Source{Inner: inner, Cache: c,
		Connector: "sf", Principal: "t1|u1", MaxStaleness: staleness}
}

// TestConcurrentHitAndRevalidateOnOneKey is the test the suite had no
// equivalent of: two goroutines on ONE cache key, one taking the hit path
// and one revalidating. Before entries were made immutable, this raced the
// hit path's read of fetchedAt against the revalidation path's write of
// it - the ordinary case for a shared cache under load, not an exotic one.
// Worthless without -race: it asserts an absence, so the detector is the
// only thing that can observe the failure.
func TestConcurrentHitAndRevalidateOnOneKey(t *testing.T) {
	c := New()
	base := time.Now()
	c.SetClock(func() time.Time { return base.Add(time.Second) })

	src := &countingSource{notModified: true}
	key := cacheKey(connector.FetchRequest{Table: "accounts"}, "t1|u1")
	c.entries[key] = &entry{rows: []connector.Row{{"id": "1"}}, etag: "v1", fetchedAt: base}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			staleness := 10 * time.Second // age 1s -> hit, reads fetchedAt
			if i%2 == 1 {
				staleness = time.Millisecond // age 1s -> revalidate, writes fetchedAt
			}
			_, _, err := srcFor(c, src, staleness).Fetch(context.Background(),
				connector.FetchRequest{Table: "accounts"})
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// TestUnsolicitedNotModifiedIsRejectedNotPanic covers the connector that
// answers 304 though no conditional request was ever sent - there is no
// stored entry to revalidate, so there is nothing to serve. This used to
// dereference a nil entry, and on the async path (server.go's detached
// goroutine) that panic took the whole process with it rather than
// failing one request.
func TestUnsolicitedNotModifiedIsRejectedNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on unsolicited 304: %v", r)
		}
	}()
	c := New()
	_, _, err := srcFor(c, &countingSource{notModified: true}, time.Minute).
		Fetch(context.Background(), connector.FetchRequest{Table: "accounts"})
	if err == nil {
		t.Fatal("expected an error naming the protocol violation, got nil")
	}
}

// TestConcurrentMissesOnOneKeyStampedeToTheConnector documents a gap
// rather than a guarantee. DESIGN.md's runbook says request coalescing
// (singleflight) absorbs a stampede; nothing implements it, so N
// concurrent misses on one cold key produce N connector calls, not 1.
// Asserting the real behaviour keeps the gap visible and makes this test
// fail loudly on the day coalescing is added - at which point the
// assertion flips to 1 and the runbook becomes true.
func TestConcurrentMissesOnOneKeyStampedeToTheConnector(t *testing.T) {
	c := New()
	release := make(chan struct{})
	src := &countingSource{blockUntil: release}

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = srcFor(c, src, time.Minute).Fetch(context.Background(),
				connector.FetchRequest{Table: "accounts"})
		}()
	}
	// Let every goroutine reach the connector before any of them returns,
	// so they are genuinely concurrent misses rather than a serial queue.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := src.calls.Load(); got != n {
		t.Fatalf("expected %d calls (no coalescing implemented); got %d - "+
			"if this dropped to 1, singleflight landed and this test should now assert that", n, got)
	}
}

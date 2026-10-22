package keypool

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

type pickStoreFactory func(t *testing.T) kvtest.ScriptStore

// expireNX is what both backends offer beyond kv.Store; the suite uses it to tell whether the rotation counter carries a TTL.
type expireNX interface {
	ExpireNX(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// runPickBatchSuite pins what one Pick costs: a single round trip carrying the breaker reads (one for prioritized while the first key is fit) and, for round-robin, the counter's INCR and EXPIRE NX.
func runPickBatchSuite(t *testing.T, factory pickStoreFactory) {
	keys := func() []*hostkey.HostKey {
		return []*hostkey.HostKey{key("a", "hA"), key("b", "hB"), key("c", "hC"), key("d", "hD")}
	}

	t.Run("round_robin_one_batch", func(t *testing.T) {
		store := kvtest.NewRecording(factory(t))
		sel := New(store, noopLogger(), frozenClock(t0), nil)
		ks := keys()
		got, err := sel.Pick(context.Background(), "rr-batch", KeySelectionRoundRobin, ks)
		if err != nil || got != ks[0] {
			t.Fatalf("pick = %v, %v; want the first key", got, err)
		}
		if trips, batches := store.RoundTrips(), store.Batches(); trips != 1 || len(batches) != 1 || batches[0] != len(ks)+2 {
			t.Fatalf("round trips = %d, batches = %v; want 1 of %d (reads, INCR, EXPIRE NX)", trips, batches, len(ks)+2)
		}
		got, _ = sel.Pick(context.Background(), "rr-batch", KeySelectionRoundRobin, ks)
		if got != ks[1] {
			t.Fatalf("second pick = %v, want the rotation to move on", got)
		}
	})

	t.Run("prioritized_reads_first_fit_key", func(t *testing.T) {
		store := kvtest.NewRecording(factory(t))
		sel := New(store, noopLogger(), frozenClock(t0), nil)
		ctx := context.Background()
		ks := keys()
		if got, err := sel.Pick(ctx, "prio-batch", KeySelectionPrioritized, ks); err != nil || got != ks[0] {
			t.Fatalf("pick = %v, %v; want the first key", got, err)
		}
		if trips, batches := store.RoundTrips(), store.Batches(); trips != 1 || batches[0] != 1 {
			t.Fatalf("round trips = %d, batches = %v; want one read in one batch", trips, batches)
		}

		sel.RecordFailure(ctx, "hA", FailureAuth, 0)
		before := store.RoundTrips()
		if got, err := sel.Pick(ctx, "prio-batch", KeySelectionPrioritized, ks); err != nil || got != ks[1] {
			t.Fatalf("pick with the first key open = %v, %v; want the second", got, err)
		}
		if trips := store.RoundTrips() - before; trips != 2 {
			t.Fatalf("round trips = %d with the first key open, want 2 (the batch and one read)", trips)
		}

		// An excluded key is skipped and the anonymous key needs no read.
		before = store.RoundTrips()
		if got, _ := sel.Pick(ctx, "prio-batch", KeySelectionPrioritized, ks, ks[:2]); got != ks[2] {
			t.Fatalf("pick past excluded keys = %v, want the third", got)
		}
		if trips := store.RoundTrips() - before; trips != 1 {
			t.Fatalf("round trips = %d past excluded keys, want 1", trips)
		}
		anon := key("anon", hostkey.AnonIDPrefix+"host")
		before = store.RoundTrips()
		if got, _ := sel.Pick(ctx, "prio-batch", KeySelectionPrioritized, append([]*hostkey.HostKey{anon}, ks...)); got != anon {
			t.Fatalf("pick = %v, want the anonymous key", got)
		}
		if trips := store.RoundTrips() - before; trips != 0 {
			t.Fatalf("round trips = %d for an anonymous first key, want 0", trips)
		}
	})

	t.Run("round_robin_counter_ttl", func(t *testing.T) {
		raw := factory(t)
		nx, ok := raw.(expireNX)
		if !ok {
			t.Fatalf("%T has no ExpireNX", raw)
		}
		sel := New(raw, noopLogger(), frozenClock(t0), nil)
		ctx := context.Background()
		counter := roundRobinKey("rr-ttl")
		if _, err := sel.Pick(ctx, "rr-ttl", KeySelectionRoundRobin, keys()); err != nil {
			t.Fatal(err)
		}
		if set, err := nx.ExpireNX(ctx, counter, time.Minute); err != nil || set {
			t.Fatalf("counter had no TTL after its first pick (set=%v, err=%v)", set, err)
		}
		// A counter that lost its TTL (a SET without one) gets one back on the next pick.
		if err := raw.Set(ctx, counter, []byte("7"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := sel.Pick(ctx, "rr-ttl", KeySelectionRoundRobin, keys()); err != nil {
			t.Fatal(err)
		}
		if set, err := nx.ExpireNX(ctx, counter, time.Minute); err != nil || set {
			t.Fatalf("counter still without TTL after a pick (set=%v, err=%v)", set, err)
		}
		if b, _ := raw.Get(ctx, counter); string(b) != "8" {
			t.Fatalf("counter = %q, want 8", b)
		}
	})
}

func TestPickBatchMem(t *testing.T) {
	runPickBatchSuite(t, func(t *testing.T) kvtest.ScriptStore {
		m := kv.NewMem()
		t.Cleanup(func() { _ = m.Close() })
		return m
	})
}

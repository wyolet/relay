package keypool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// pod is one process's selector and pick state over a shared store; picks are recorded, refreshes go to the store unrecorded as they would on the post-flight client.
type pod struct {
	sel   *Selector
	state *PickState
	rec   *kvtest.Recording
}

func newPod(store kvtest.ScriptStore, now *time.Time) pod {
	clock := func() time.Time { return *now }
	rec := kvtest.NewRecording(store)
	ps := NewPickState(store, time.Second, noopLogger()).WithClock(clock)
	return pod{sel: New(rec, noopLogger(), clock, nil).WithPickState(ps), state: ps, rec: rec}
}

// trips runs fn and returns the store round trips it made on the pick path.
func (p pod) trips(fn func()) int {
	before := p.rec.RoundTrips()
	fn()
	return p.rec.RoundTrips() - before
}

// runPickStateSuite pins picks served from a pod's memory: a warm pick makes no store call, a cold or stale record is read through once, a pod's own transitions hold at once, and another pod's reach it after one refresh.
func runPickStateSuite(t *testing.T, factory pickStoreFactory) {
	ctx := context.Background()
	keys := func() []*hostkey.HostKey {
		return []*hostkey.HostKey{key("a", "hA"), key("b", "hB"), key("c", "hC")}
	}

	for _, algo := range []KeySelection{KeySelectionPrioritized, KeySelectionRoundRobin, KeySelectionLeastRecentlyUsed} {
		t.Run(string(algo)+"_warm_pick_makes_no_store_call", func(t *testing.T) {
			now := t0
			p := newPod(factory(t), &now)
			ks := keys()
			if n := p.trips(func() { mustPick(t, p.sel, algo, ks) }); n != 1 {
				t.Fatalf("cold pick took %d round trips, want the one read-through", n)
			}
			for range 4 {
				if n := p.trips(func() { mustPick(t, p.sel, algo, ks) }); n != 0 {
					t.Fatalf("warm pick took %d round trips, want 0 (%v)", n, p.rec.Batches())
				}
			}
		})
	}

	t.Run("round_robin_rotates_in_memory", func(t *testing.T) {
		now := t0
		store := factory(t)
		p := newPod(store, &now)
		ks := keys()
		var got []string
		for range 2 * len(ks) {
			got = append(got, mustPick(t, p.sel, KeySelectionRoundRobin, ks).KeyHash)
		}
		if strings.Join(got, ",") != "hA,hB,hC,hA,hB,hC" {
			t.Fatalf("rotation = %v, want each key in turn", got)
		}
		if _, err := store.Get(ctx, roundRobinKey("scope")); err == nil {
			t.Fatal("the rotation counter was written to the store")
		}
	})

	t.Run("cold_read_through_once", func(t *testing.T) {
		now := t0
		p := newPod(factory(t), &now)
		ks := keys()[:1]
		n := p.trips(func() {
			for range 5 {
				mustPick(t, p.sel, KeySelectionPrioritized, ks)
			}
		})
		if n != 1 {
			t.Fatalf("five picks of a cold key took %d round trips, want the first one's read", n)
		}
	})

	t.Run("stale_record_is_read_through", func(t *testing.T) {
		now := t0
		p := newPod(factory(t), &now)
		ks := keys()[:1]
		mustPick(t, p.sel, KeySelectionPrioritized, ks)
		// No refresh for longer than two intervals: the copy is too old to trust.
		now = now.Add(2*time.Second + time.Millisecond)
		if n := p.trips(func() { mustPick(t, p.sel, KeySelectionPrioritized, ks) }); n != 1 {
			t.Fatalf("pick on a stale record took %d round trips, want a read-through", n)
		}
	})

	t.Run("own_failure_holds_at_once", func(t *testing.T) {
		now := t0
		p := newPod(factory(t), &now)
		ks := keys()
		mustPick(t, p.sel, KeySelectionPrioritized, ks)
		p.sel.RecordFailure(ctx, "hA", FailureAuth, 0)
		var got *hostkey.HostKey
		if n := p.trips(func() { got = mustPick(t, p.sel, KeySelectionPrioritized, ks) }); got.KeyHash != "hB" || n != 1 {
			t.Fatalf("pick after this pod's failure = %s in %d round trips; want hB, read through once", got.KeyHash, n)
		}
	})

	t.Run("other_pod_failure_after_one_refresh", func(t *testing.T) {
		now := t0
		store := factory(t)
		a, b := newPod(store, &now), newPod(store, &now)
		ks := keys()
		mustPick(t, a.sel, KeySelectionPrioritized, ks)
		mustPick(t, b.sel, KeySelectionPrioritized, ks)

		a.sel.RecordFailure(ctx, "hA", FailureAuth, 0)
		if got := mustPick(t, a.sel, KeySelectionPrioritized, ks); got.KeyHash != "hB" {
			t.Fatalf("pod A picked %s, want hB past its own failure", got.KeyHash)
		}
		// Within the interval pod B still holds the closed record.
		if got := mustPick(t, b.sel, KeySelectionPrioritized, ks); got.KeyHash != "hA" {
			t.Fatalf("pod B picked %s before its refresh, want the stale hA", got.KeyHash)
		}
		now = now.Add(time.Second)
		b.state.Refresh(ctx)
		if got := mustPick(t, b.sel, KeySelectionPrioritized, ks); got.KeyHash != "hB" {
			t.Fatalf("pod B picked %s after its refresh, want hB", got.KeyHash)
		}

		// A cleared record reaches B the same way.
		if err := ClearCircuit(ctx, store, "hA"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		b.state.Refresh(ctx)
		if got := mustPick(t, b.sel, KeySelectionPrioritized, ks); got.KeyHash != "hA" {
			t.Fatalf("pod B picked %s after the record was cleared, want hA", got.KeyHash)
		}
	})
}

func mustPick(t *testing.T, sel *Selector, algo KeySelection, ks []*hostkey.HostKey) *hostkey.HostKey {
	t.Helper()
	k, err := sel.Pick(context.Background(), "scope", algo, ks)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	return k
}

func TestPickStateMem(t *testing.T) {
	runPickStateSuite(t, func(t *testing.T) kvtest.ScriptStore {
		m := kv.NewMem()
		t.Cleanup(func() { _ = m.Close() })
		RegisterScripts(m)
		return m
	})
}

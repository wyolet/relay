package engine_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

func upsert(id string, ver int64) engine.Event {
	return engine.Event{Ref: refOf(kItem, id), Op: engine.OpUpsert, Version: ver}
}

func TestNotifiedEventsPublishOneGenerationPerBatch(t *testing.T) {
	r := newRig(t, engine.Options{Debounce: 100 * time.Millisecond})
	r.load()
	r.items.set(&item{ID: "i1", Ver: 1}, &item{ID: "i2", Ver: 1}, &item{ID: "i3", Ver: 1})
	r.e.Notify(upsert("i1", 1))
	r.e.Notify(upsert("i2", 1))
	r.e.Notify(upsert("i3", 1))

	s := r.nextPub()
	if s.Gen != 2 || s.Inputs != 3 {
		t.Fatalf("batch published gen %d with %d inputs, want gen 2 with 3", s.Gen, s.Inputs)
	}
	for _, id := range []string{"i1", "i2", "i3"} {
		if engine.Get[item](r.view(), id) == nil {
			t.Fatalf("%s missing after the batch", id)
		}
	}
	r.expectNoPub(250 * time.Millisecond)
}

func TestNotifyCoalescesEventsForOneRow(t *testing.T) {
	r := newRig(t, engine.Options{Debounce: 100 * time.Millisecond})
	r.items.set(&item{ID: "i1", Ver: 1})
	r.load()
	r.items.set(&item{ID: "i1", Ver: 3, Val: 3})
	r.e.Notify(upsert("i1", 2))
	r.e.Notify(upsert("i1", 3))
	r.e.Notify(upsert("i1", 2))

	s := r.nextPub()
	if s.Inputs != 1 {
		t.Fatalf("inputs = %d, want 1 for three events on one row", s.Inputs)
	}
	if n := r.items.getCount("i1"); n != 1 {
		t.Fatalf("fetched i1 %d times, want 1", n)
	}
	if got := engine.Get[item](r.view(), "i1"); got == nil || got.Val != 3 {
		t.Fatalf("i1 = %+v, want Val 3", got)
	}
}

func TestEventAtHeldVersionIsDroppedUnfetched(t *testing.T) {
	r := newRig(t, engine.Options{Debounce: 50 * time.Millisecond})
	r.items.set(&item{ID: "i1", Ver: 5})
	r.load()
	r.items.set(&item{ID: "i2", Ver: 1})
	r.e.Notify(upsert("i1", 5))
	r.e.Notify(upsert("i1", 4))
	r.e.Notify(upsert("i2", 0))

	r.nextPub()
	if n := r.items.getCount("i1"); n != 0 {
		t.Fatalf("stale event fetched i1 %d times, want 0", n)
	}
	if n := r.items.getCount("i2"); n != 1 {
		t.Fatalf("version-0 event fetched i2 %d times, want 1", n)
	}
}

func TestVersionZeroEventAlwaysFetches(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 5})
	r.load()
	r.items.set(&item{ID: "i1", Ver: 5, Val: 9})
	r.e.Notify(upsert("i1", 0))
	eventually(t, "fetch of i1", func() bool { return r.items.getCount("i1") == 1 })
}

func TestNotifiedDeleteRemovesRow(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1})
	r.load()
	r.items.remove("i1")
	r.e.Notify(engine.Event{Ref: refOf(kItem, "i1"), Op: engine.OpDelete})
	r.nextPub()
	if engine.Get[item](r.view(), "i1") != nil {
		t.Fatal("deleted row still in the view")
	}
}

func TestFetchOfMissingRowAppliesDelete(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1})
	r.load()
	r.items.remove("i1")
	r.e.Notify(upsert("i1", 0))
	r.nextPub()
	if engine.Get[item](r.view(), "i1") != nil {
		t.Fatal("row the source no longer has is still in the view")
	}
}

func resyncRig(t *testing.T) (*rig, chan int) {
	changed := make(chan int, 8)
	r := newRig(t, engine.Options{Hooks: engine.Hooks{
		OnResync: func(n int, _ time.Duration) { changed <- n },
	}})
	return r, changed
}

func waitInt(t *testing.T, ch chan int) int {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(3 * time.Second):
		t.Fatal("hook not called within 3s")
		return 0
	}
}

func TestResyncWithoutChangesDoesNotPublish(t *testing.T) {
	r, changed := resyncRig(t)
	r.items.set(&item{ID: "i1", Ver: 1}, &item{ID: "i2", Ver: 1})
	r.load()
	if err := r.e.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if n := waitInt(t, changed); n != 0 {
		t.Fatalf("OnResync changed = %d, want 0", n)
	}
	if g := r.view().Gen(); g != 1 {
		t.Fatalf("gen after no-op Resync = %d, want 1", g)
	}
	r.expectNoPub(50 * time.Millisecond)
}

func TestResyncAppliesDifferencesInOnePublish(t *testing.T) {
	r, changed := resyncRig(t)
	r.items.set(&item{ID: "keep", Ver: 1}, &item{ID: "edit", Ver: 1}, &item{ID: "gone", Ver: 1})
	r.load()

	r.items.set(&item{ID: "edit", Ver: 2, Val: 2}, &item{ID: "new", Ver: 1})
	r.items.remove("gone")
	if err := r.e.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if n := waitInt(t, changed); n != 3 {
		t.Fatalf("OnResync changed = %d, want 3", n)
	}
	if s := r.nextPub(); s.Gen != 2 {
		t.Fatalf("Resync published gen %d, want 2", s.Gen)
	}
	r.expectNoPub(50 * time.Millisecond)
	v := r.view()
	if got := engine.Get[item](v, "edit"); got == nil || got.Val != 2 {
		t.Fatalf("edited row = %+v, want Val 2", got)
	}
	if engine.Get[item](v, "new") == nil || engine.Get[item](v, "gone") != nil {
		t.Fatal("Resync did not add the new row and remove the missing one")
	}
}

func TestResyncComparesValuesWhenVersionIsZero(t *testing.T) {
	r, changed := resyncRig(t)
	r.items.set(&item{ID: "i1"})
	r.load()

	r.items.set(&item{ID: "i1"})
	if err := r.e.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if n := waitInt(t, changed); n != 0 {
		t.Fatalf("equal unversioned row counted as %d changes, want 0", n)
	}
	r.expectNoPub(50 * time.Millisecond)

	r.items.set(&item{ID: "i1", Val: 1})
	if err := r.e.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if n := waitInt(t, changed); n != 1 {
		t.Fatalf("changed unversioned row counted as %d changes, want 1", n)
	}
	if got := engine.Get[item](r.view(), "i1"); got == nil || got.Val != 1 {
		t.Fatalf("i1 = %+v, want Val 1", got)
	}
}

func TestPublishHookMayApplyAndDelete(t *testing.T) {
	var e *engine.Engine
	var mu sync.Mutex
	var gens []engine.Generation
	r := newRig(t, engine.Options{Hooks: engine.Hooks{
		OnPublish: func(v *engine.View, s engine.PublishStats) {
			mu.Lock()
			gens = append(gens, s.Gen)
			mu.Unlock()
			switch s.Gen {
			case 2:
				_ = engine.Apply(e, &item{ID: "from-hook", Ver: 1})
			case 3:
				_ = engine.Delete(e, refOf(kItem, "first"))
			}
		},
	}})
	e = r.e
	r.load()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = engine.Apply(r.e, &item{ID: "first", Ver: 1})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply from a publish hook deadlocked")
	}
	eventually(t, "gen 4", func() bool { return r.view().Gen() == 4 })
	v := r.view()
	if engine.Get[item](v, "from-hook") == nil || engine.Get[item](v, "first") != nil {
		t.Fatal("writes made from the hook are not in the view")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, g := range gens {
		if g != engine.Generation(i+1) {
			t.Fatalf("hook saw generations %v, want 1,2,3,4 in order", gens)
		}
	}
}

func TestPublishHookSeesGenerationsInOrder(t *testing.T) {
	var mu sync.Mutex
	var gens []engine.Generation
	var mismatch bool
	r := newRig(t, engine.Options{Hooks: engine.Hooks{
		OnPublish: func(v *engine.View, s engine.PublishStats) {
			mu.Lock()
			defer mu.Unlock()
			gens = append(gens, s.Gen)
			if v.Gen() != s.Gen {
				mismatch = true
			}
		},
	}})
	r.load()

	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 10 {
				row := &item{ID: fmt.Sprintf("g%d-%d", g, k), Ver: 1}
				r.items.set(row)
				_ = engine.Apply(r.e, row)
			}
		}()
	}
	wg.Wait()
	eventually(t, "hooks to catch up", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gens) > 0 && gens[len(gens)-1] == r.view().Gen()
	})
	mu.Lock()
	defer mu.Unlock()
	if mismatch {
		t.Fatal("hook got a view whose Gen differs from its stats")
	}
	for i, g := range gens {
		if g != engine.Generation(i+1) {
			t.Fatalf("hook generations %v are not consecutive from 1", gens)
		}
	}
}

func TestCloseDrainsQueuedEventsThenStopsPublishing(t *testing.T) {
	r := newRig(t, engine.Options{Debounce: time.Hour})
	r.items.set(&item{ID: "i1", Ver: 1})
	r.load()

	r.items.set(&item{ID: "i1", Ver: 2, Val: 7})
	r.e.Notify(upsert("i1", 2))
	r.close()
	if got := engine.Get[item](r.view(), "i1"); got == nil || got.Val != 7 {
		t.Fatalf("queued event not drained by Close: i1 = %+v", got)
	}
	select {
	case s := <-r.pubs:
		if s.Gen != 2 {
			t.Fatalf("Close published gen %d, want 2", s.Gen)
		}
	default:
		t.Fatal("hook for the drained batch had not run when Close returned")
	}

	gen := r.view().Gen()
	r.items.set(&item{ID: "i2", Ver: 1})
	r.e.Notify(upsert("i2", 1))
	go func() { _ = engine.Apply(r.e, &item{ID: "i3", Ver: 1}) }()
	r.expectNoPub(100 * time.Millisecond)
	if g := r.view().Gen(); g != gen {
		t.Fatalf("gen moved from %d to %d after Close", gen, g)
	}
}

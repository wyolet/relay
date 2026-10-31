package engine_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

func TestViewBeforeLoadIsGenerationZero(t *testing.T) {
	r := newRig(t, engine.Options{})
	if v := r.e.View(); v == nil || v.Gen() != 0 {
		t.Fatalf("pre-boot view = %v, want generation 0", v)
	}
	r.load()
	if g := r.view().Gen(); g != 1 {
		t.Fatalf("gen after Load = %d, want 1", g)
	}
}

func TestEachApplyPublishesTheNextGeneration(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.e.Register(ctlByID(r))
	r.load()
	for want := engine.Generation(2); want <= 4; want++ {
		r.putParent(&parent{ID: "p1", Ver: int64(want), Val: int(want)})
		s := r.nextPub()
		if s.Gen != want || r.view().Gen() != want {
			t.Fatalf("publish gen = %d, view gen = %d, want %d", s.Gen, r.view().Gen(), want)
		}
		if s.Inputs != 1 {
			t.Fatalf("gen %d inputs = %d, want 1", want, s.Inputs)
		}
	}
}

func TestPublishedViewIsNotChangedByLaterWrites(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	r.e.Register(ctlByID(r))
	r.load()

	old := r.view()
	oldParent := engine.Get[parent](old, "p1")
	oldOut := viewA(old, "i1")
	if oldOut == nil || oldOut.V != 1 {
		t.Fatalf("i1 output = %+v, want V=1", oldOut)
	}

	r.putParent(&parent{ID: "p1", Ver: 2, Val: 2})
	r.putItem(&item{ID: "i2", Ver: 1, Parent: "p1"})
	r.delItem("i1")

	if old.Gen() != 1 {
		t.Fatalf("old view gen = %d, want 1", old.Gen())
	}
	if got := engine.Get[parent](old, "p1"); got != oldParent || got.Val != 1 {
		t.Fatalf("old view parent = %+v, want the original pointer with Val 1", got)
	}
	if got := viewA(old, "i1"); got != oldOut || got.V != 1 {
		t.Fatalf("old view output = %+v, want the original pointer with V 1", got)
	}
	if engine.Get[item](old, "i2") != nil {
		t.Fatal("old view sees a row added later")
	}
	n := 0
	for range engine.All[item](old) {
		n++
	}
	if n != 1 {
		t.Fatalf("old view holds %d items, want 1", n)
	}

	cur := r.view()
	if engine.Get[item](cur, "i1") != nil || viewA(cur, "i1") != nil {
		t.Fatal("current view still holds the deleted row or its output")
	}
	if got := viewA(cur, "i2"); got == nil || got.V != 2 {
		t.Fatalf("current i2 output = %+v, want V=2", got)
	}
}

func TestViewsReadConcurrentlyWithWrites(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 0})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	r.e.Register(ctlByID(r))
	r.load()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			v := r.view()
			for it := range engine.All[item](v) {
				_ = it.Val
			}
			if o := viewA(v, "i1"); o != nil {
				_ = o.V
			}
			_ = v.AllIssues()
		}
	}()
	for n := 2; n < 40; n++ {
		r.putParent(&parent{ID: "p1", Ver: int64(n), Val: n})
	}
	close(stop)
	wg.Wait()
}

func TestPublishStatsCountReconcileCalls(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"}, &item{ID: "i2", Ver: 1, Parent: "p1"}, &item{ID: "i3", Ver: 1, Parent: "p2"})
	r.e.Register(ctlByID(r))
	r.load()

	r.putParent(&parent{ID: "p1", Ver: 2, Val: 1})
	s := r.nextPub()
	calls := r.calls.count("by-id", "i1") + r.calls.count("by-id", "i2") + r.calls.count("by-id", "i3")
	if s.Reconciled != calls || calls != 2 {
		t.Fatalf("stats Reconciled = %d, reconcile calls = %d, want 2 for the two readers of p1", s.Reconciled, calls)
	}
	if s.Inputs != 1 || s.Rounds < 1 {
		t.Fatalf("stats = %+v, want 1 input and at least one round", s)
	}
}

func TestNotifyNeverBlocks(t *testing.T) {
	r := newRig(t, engine.Options{Debounce: time.Hour})
	r.load()
	start := time.Now()
	for i := range 10000 {
		r.e.Notify(upsert(fmt.Sprintf("i%d", i), 0))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("10k Notify calls took %v", d)
	}
}

func TestRegistrationPanicsAfterLoad(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.load()
	mustPanic := func(what string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s after Load did not panic", what)
			}
		}()
		f()
	}
	type late struct{ ID string }
	mustPanic("RegisterKind", func() { engine.RegisterKind(r.e, engine.KindSpec[late]{Kind: "late"}) })
	mustPanic("Register", func() { r.e.Register(engine.NewController("late", kC, func(*engine.Txn, string) error { return nil })) })
}

func TestAllIteratesInIDOrder(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "c", Ver: 1}, &item{ID: "a", Ver: 1}, &item{ID: "b", Ver: 1})
	r.load()
	var ids []string
	for it := range engine.All[item](r.view()) {
		ids = append(ids, it.ID)
	}
	if len(ids) != 3 || ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Fatalf("All order = %v, want [a b c]", ids)
	}
}

package engine_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

type window struct{ hits int }

func TestCellsLiveWhileLinked(t *testing.T) {
	reg := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	var made atomic.Int32
	f := newFixture(t, engine.Options{Cells: reg}, func(f *fixture) {
		engine.RegisterKind(f.e, engine.KindSpec[*window]{Kind: "teamwindow"})
		c := engine.NewController("team-window", "teamwindow", func(tx *engine.Txn, id string) error {
			tm := engine.Get[team](tx, id)
			if tm == nil {
				engine.Drop(tx)
				return nil
			}
			w := cell.Acquire(reg, tx, cell.Key{Type: "window", ID: id}, func() *window { made.Add(1); return &window{} })
			engine.Put(tx, &w)
			return nil
		})
		engine.Watch(c, func(t *team) []string { return []string{t.ID} })
		f.e.Register(c)
	})
	w1 := *engine.Get[*window](f.view(), "t1")
	w1.hits = 7

	// A rebuild acquires the same cell: the count survives.
	mustApply(t, f.e, &team{ID: "t1", Name: "alpha", Limit: 1})
	if got := *engine.Get[*window](f.view(), "t1"); got != w1 || got.hits != 7 || made.Load() != 2 {
		t.Fatalf("rebuild did not re-link the same cell (made %d)", made.Load())
	}
	if n := reg.Expire(time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("linked cells expired: %d", n)
	}

	// Dropping the only output that links a cell starts its idle clock.
	if err := engine.Delete(f.e, engine.Ref{Kind: "team", ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if n := reg.Expire(time.Now().Add(30 * time.Second)); n != 0 {
		t.Fatalf("expired before IdleTTL: %d", n)
	}
	if n := reg.Expire(time.Now().Add(2 * time.Minute)); n != 1 || reg.Len() != 1 {
		t.Fatalf("Expire = %d, Len = %d", n, reg.Len())
	}
}

func TestCellsAcquiredByAFailedReconcileStayUnlinked(t *testing.T) {
	reg := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	f := newFixture(t, engine.Options{Cells: reg, Debounce: time.Hour}, func(f *fixture) {
		engine.RegisterKind(f.e, engine.KindSpec[*window]{Kind: "teamwindow"})
		c := engine.NewController("team-window", "teamwindow", func(tx *engine.Txn, id string) error {
			tm := engine.Get[team](tx, id)
			w := cell.Acquire(reg, tx, cell.Key{Type: "window", ID: tm.Name}, func() *window { return &window{} })
			if tm.Limit < 0 {
				return errBoom
			}
			engine.Put(tx, &w)
			return nil
		})
		engine.Watch(c, func(t *team) []string { return []string{t.ID} })
		f.e.Register(c)
	})
	mustApply(t, f.e, &team{ID: "t1", Name: "renamed", Limit: -1})
	if n := reg.Expire(time.Now().Add(2 * time.Minute)); n != 1 {
		t.Fatalf("Expire = %d, want only the failed reconcile's cell", n)
	}
	if engine.Get[*window](f.view(), "t1") == nil {
		t.Fatal("previous output not kept")
	}
}

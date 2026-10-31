package engine_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

const idleTTL = time.Minute

type dropLog struct {
	mu   sync.Mutex
	keys []cell.Key
}

func (d *dropLog) record(k cell.Key, _ any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys = append(d.keys, k)
}

func (d *dropLog) list() []cell.Key {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]cell.Key(nil), d.keys...)
}

// cellRig links one counter cell per item into its outA.
func cellRig(t *testing.T) (*rig, *cell.Registry, *dropLog) {
	drops := &dropLog{}
	reg := cell.NewRegistry(cell.Options{IdleTTL: idleTTL, OnDrop: drops.record})
	r := newRig(t, engine.Options{Cells: reg})
	c := engine.NewController("counted", kA, func(tx *engine.Txn, id string) error {
		it := engine.Get[item](tx, id)
		if it == nil {
			engine.Drop(tx)
			return nil
		}
		ctr := cell.Acquire(reg, tx, cell.Key{Type: "ctr", ID: id}, func() *atomic.Int64 { return new(atomic.Int64) })
		engine.Put(tx, &outA{V: it.Val, Cell: ctr})
		return nil
	})
	engine.Watch(c, selfKey)
	r.e.Register(c)
	return r, reg, drops
}

func TestLinkedCellsNeverExpire(t *testing.T) {
	r, reg, drops := cellRig(t)
	r.items.set(&item{ID: "i1", Ver: 1}, &item{ID: "i2", Ver: 1})
	r.load()
	if n := reg.Expire(time.Now().Add(24 * time.Hour)); n != 0 {
		t.Fatalf("Expire dropped %d linked cells (%v)", n, drops.list())
	}
	if reg.Len() != 2 {
		t.Fatalf("Len = %d, want 2", reg.Len())
	}
}

func TestUnlinkedCellExpiresAfterIdleTTL(t *testing.T) {
	r, reg, drops := cellRig(t)
	r.items.set(&item{ID: "i1", Ver: 1}, &item{ID: "i2", Ver: 1})
	r.load()

	r.delItem("i2")
	if n := reg.Expire(time.Now()); n != 0 {
		t.Fatalf("Expire before IdleTTL dropped %d cells", n)
	}
	if n := reg.Expire(time.Now().Add(idleTTL + time.Second)); n != 1 {
		t.Fatalf("Expire after IdleTTL dropped %d cells, want 1", n)
	}
	if got := drops.list(); len(got) != 1 || got[0] != (cell.Key{Type: "ctr", ID: "i2"}) {
		t.Fatalf("OnDrop keys = %v, want [ctr/i2]", got)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}
}

func TestRebuildRelinksTheSameCell(t *testing.T) {
	r, _, _ := cellRig(t)
	r.items.set(&item{ID: "i1", Ver: 1})
	r.load()
	first := viewA(r.view(), "i1").Cell
	first.Store(42)

	r.putItem(&item{ID: "i1", Ver: 2, Val: 1})
	got := viewA(r.view(), "i1")
	if got.V != 1 || got.Cell != first || got.Cell.Load() != 42 {
		t.Fatalf("rebuilt output cell = %p (%d), want %p holding 42", got.Cell, got.Cell.Load(), first)
	}
}

func TestCellRelinkedBeforeExpiryIsKept(t *testing.T) {
	r, reg, _ := cellRig(t)
	r.items.set(&item{ID: "i1", Ver: 1})
	r.load()
	first := viewA(r.view(), "i1").Cell

	r.delItem("i1")
	r.putItem(&item{ID: "i1", Ver: 2})
	if n := reg.Expire(time.Now().Add(idleTTL + time.Second)); n != 0 {
		t.Fatalf("Expire dropped %d relinked cells", n)
	}
	if viewA(r.view(), "i1").Cell != first {
		t.Fatal("relinked output got a fresh cell instead of the kept one")
	}
}

func TestUnchangedOutputsKeepTheirLinks(t *testing.T) {
	r, reg, _ := cellRig(t)
	r.items.set(&item{ID: "i1", Ver: 1}, &item{ID: "i2", Ver: 1})
	r.load()

	// Batches that never reconcile i1 must not unlink its cell.
	r.putItem(&item{ID: "i2", Ver: 2})
	r.putItem(&item{ID: "i2", Ver: 3})
	if n := reg.Expire(time.Now().Add(24 * time.Hour)); n != 0 {
		t.Fatalf("Expire dropped %d cells of outputs left untouched", n)
	}
}

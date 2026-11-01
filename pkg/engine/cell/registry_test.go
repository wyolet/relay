package cell_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine/cell"
)

type counter struct{ n atomic.Int64 }

// keyLog is a Linker that records what it was asked to link.
type keyLog struct {
	mu   sync.Mutex
	keys []cell.Key
}

func (l *keyLog) Link(k cell.Key) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, k)
}

func newCounter() *counter { return &counter{} }

func TestAcquireReturnsTheSameAddressForAKey(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	k := cell.Key{Type: "window", ID: "w1"}
	a := cell.Acquire(r, nil, k, newCounter)
	a.n.Store(5)
	b := cell.Acquire(r, nil, k, func() *counter { t.Fatal("mk called for an existing cell"); return nil })
	if a != b || b.n.Load() != 5 {
		t.Fatalf("second Acquire returned %p, want %p with its state", b, a)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestAcquireCreatesOnceUnderConcurrency(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	k := cell.Key{Type: "window", ID: "w1"}
	var made atomic.Int32
	start := make(chan struct{})
	got := make([]*counter, 32)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i] = cell.Acquire(r, nil, k, func() *counter { made.Add(1); return &counter{} })
		}()
	}
	close(start)
	wg.Wait()
	if made.Load() != 1 {
		t.Fatalf("mk ran %d times, want 1", made.Load())
	}
	for _, c := range got {
		if c != got[0] {
			t.Fatal("concurrent Acquire returned different cells for one key")
		}
	}
}

func TestKeysDifferingOnlyInTypeAreSeparateCells(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	a := cell.Acquire(r, nil, cell.Key{Type: "breaker", ID: "x"}, newCounter)
	b := cell.Acquire(r, nil, cell.Key{Type: "cooldown", ID: "x"}, newCounter)
	if a == b || r.Len() != 2 {
		t.Fatalf("same id, different type gave one cell (Len %d)", r.Len())
	}
}

func TestAcquireLinksTheCallersOutput(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	l := &keyLog{}
	k := cell.Key{Type: "window", ID: "w1"}
	cell.Acquire(r, l, k, newCounter)
	cell.Acquire(r, l, k, newCounter)
	if len(l.keys) != 2 || l.keys[0] != k || l.keys[1] != k {
		t.Fatalf("Linker saw %v, want the key on every Acquire, hit or miss", l.keys)
	}
}

func TestUnlinkedCellIsDroppedAfterIdleTTL(t *testing.T) {
	var dropped []cell.Key
	var values []any
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute, OnDrop: func(k cell.Key, v any) {
		dropped = append(dropped, k)
		values = append(values, v)
	}})
	k := cell.Key{Type: "window", ID: "w1"}
	c := cell.Acquire(r, nil, k, newCounter)

	if n := r.Expire(time.Now()); n != 0 {
		t.Fatalf("Expire before IdleTTL dropped %d", n)
	}
	if n := r.Expire(time.Now().Add(time.Minute + time.Second)); n != 1 {
		t.Fatalf("Expire after IdleTTL dropped %d, want 1", n)
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
	if len(dropped) != 1 || dropped[0] != k || values[0] != any(c) {
		t.Fatalf("OnDrop got %v / %v, want the key and the cell value", dropped, values)
	}

	fresh := cell.Acquire(r, nil, k, newCounter)
	if fresh == c {
		t.Fatal("Acquire after expiry returned the dropped cell")
	}
}

func TestPinnedCellNeverExpires(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	k := cell.Key{Type: "revoked", ID: ""}
	r.Pin(k)
	c := cell.Acquire(r, nil, k, newCounter)
	if n := r.Expire(time.Now().Add(24 * time.Hour)); n != 0 {
		t.Fatalf("Expire dropped %d pinned cells", n)
	}
	if cell.Acquire(r, nil, k, newCounter) != c {
		t.Fatal("pinned cell was replaced")
	}
}

func TestRegistryAsLinkerPinsTheCell(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	k := cell.Key{Type: "global", ID: "g"}
	cell.Acquire(r, r, k, newCounter)
	if n := r.Expire(time.Now().Add(24 * time.Hour)); n != 0 {
		t.Fatalf("Expire dropped %d cells acquired with the registry as linker", n)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestPinAppliesOnlyToItsKey(t *testing.T) {
	r := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	r.Pin(cell.Key{Type: "a", ID: "1"})
	cell.Acquire(r, nil, cell.Key{Type: "a", ID: "1"}, newCounter)
	cell.Acquire(r, nil, cell.Key{Type: "a", ID: "2"}, newCounter)
	if n := r.Expire(time.Now().Add(time.Hour)); n != 1 {
		t.Fatalf("Expire dropped %d, want only the unpinned cell", n)
	}
}

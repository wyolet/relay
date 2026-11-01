package cell

import (
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine/internal/celllink"
)

type counter struct{ n int }

type linkRecorder struct{ keys []Key }

func (l *linkRecorder) Link(k Key) { l.keys = append(l.keys, k) }

func publish(r *Registry, linked, unlinked []Key, now time.Time) {
	celllink.Publish(r, linked, unlinked, now)
}

func TestAcquireCreatesOnceAndLinks(t *testing.T) {
	r := NewRegistry(Options{IdleTTL: time.Minute})
	k := Key{"window", "a"}
	made := 0
	mk := func() *counter { made++; return &counter{} }
	var l linkRecorder
	a := Acquire(r, &l, k, mk)
	b := Acquire(r, &l, k, mk)
	if a != b || made != 1 {
		t.Fatalf("same key gave %p %p, mk ran %d times", a, b, made)
	}
	if len(l.keys) != 2 || l.keys[0] != k {
		t.Fatalf("links = %v", l.keys)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d", r.Len())
	}
	if Acquire(r, nil, Key{"breaker", "a"}, mk) == a {
		t.Fatal("different type, same id shared a cell")
	}
}

func TestAcquireConcurrentSingleFlight(t *testing.T) {
	r := NewRegistry(Options{})
	k := Key{"window", "x"}
	var wg sync.WaitGroup
	got := make([]*counter, 32)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = Acquire(r, nil, k, func() *counter { return &counter{} })
		}()
	}
	wg.Wait()
	for _, c := range got {
		if c != got[0] {
			t.Fatal("concurrent Acquire returned different cells")
		}
	}
}

func TestAcquireTypeMismatchPanics(t *testing.T) {
	r := NewRegistry(Options{})
	k := Key{"window", "x"}
	Acquire(r, nil, k, func() *counter { return &counter{} })
	defer func() {
		if recover() == nil {
			t.Fatal("no panic on type mismatch")
		}
	}()
	Acquire(r, nil, k, func() *string { return new(string) })
}

func TestExpireDropsOnlyIdleUnlinked(t *testing.T) {
	var droppedKeys []Key
	r := NewRegistry(Options{IdleTTL: time.Minute, OnDrop: func(k Key, v any) {
		if _, ok := v.(*counter); !ok {
			t.Errorf("OnDrop got %T", v)
		}
		droppedKeys = append(droppedKeys, k)
	}})
	linked, idle, pinned := Key{"w", "linked"}, Key{"w", "idle"}, Key{"w", "pinned"}
	r.Pin(pinned)
	for _, k := range []Key{linked, idle, pinned} {
		Acquire(r, nil, k, func() *counter { return &counter{} })
	}
	now := time.Now()
	publish(r, []Key{linked}, nil, now)

	if n := r.Expire(now); n != 0 {
		t.Fatalf("fresh cells expired: %d", n)
	}
	if n := r.Expire(now.Add(2 * time.Minute)); n != 1 {
		t.Fatalf("Expire dropped %d, want 1", n)
	}
	if len(droppedKeys) != 1 || droppedKeys[0] != idle || r.Len() != 2 {
		t.Fatalf("dropped %v, Len %d", droppedKeys, r.Len())
	}

	// Unlinking starts the idle clock at publish time.
	later := now.Add(3 * time.Minute)
	publish(r, nil, []Key{linked}, later)
	if n := r.Expire(later.Add(30 * time.Second)); n != 0 {
		t.Fatalf("expired before IdleTTL: %d", n)
	}
	if n := r.Expire(later.Add(time.Minute)); n != 1 || r.Len() != 1 {
		t.Fatalf("Expire = %d, Len %d", n, r.Len())
	}
}

func TestReacquireAfterExpireIsFresh(t *testing.T) {
	r := NewRegistry(Options{})
	k := Key{"w", "a"}
	a := Acquire(r, nil, k, func() *counter { return &counter{n: 1} })
	r.Expire(time.Now().Add(time.Second))
	b := Acquire(r, nil, k, func() *counter { return &counter{n: 2} })
	if a == b || b.n != 2 {
		t.Fatal("expired cell was returned again")
	}
}

func TestRegistryAsLinkerPins(t *testing.T) {
	r := NewRegistry(Options{})
	k := Key{"revoked", ""}
	Acquire(r, r, k, func() *counter { return &counter{} })
	if n := r.Expire(time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("self-linked cell expired: %d", n)
	}
}

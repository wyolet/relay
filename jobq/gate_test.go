package jobq

import (
	"context"
	"testing"
	"time"
)

// waiter starts an acquire that is expected to block, and reports when it returns.
func waiter(t *testing.T, g *gate) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- g.acquire(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("acquire on a full gate returned %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	return done
}

func admitted(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not admitted")
	}
}

func wantGate(t *testing.T, g *gate, inUse, limit, available int) {
	t.Helper()
	gotUse, gotLimit := g.stats()
	if gotUse != inUse || gotLimit != limit || g.available() != available {
		t.Fatalf("gate in use %d of %d, %d available; want %d of %d, %d available", gotUse, gotLimit, g.available(), inUse, limit, available)
	}
}

func TestGate_ReleaseAdmitsOneWaiter(t *testing.T) {
	g := newGate(2)
	for range 2 {
		if err := g.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wantGate(t, g, 2, 2, 0)
	first, second := waiter(t, g), waiter(t, g)

	g.release()
	select {
	case err := <-first:
		first = nil
		if err != nil {
			t.Fatal(err)
		}
	case err := <-second:
		second = nil
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no waiter was admitted after a release")
	}
	time.Sleep(30 * time.Millisecond)
	wantGate(t, g, 2, 2, 0)
	g.release()
	if first == nil {
		first = second
	}
	admitted(t, first)
	wantGate(t, g, 2, 2, 0)
}

func TestGate_AcquireRespectsContext(t *testing.T) {
	g := newGate(1)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// A free slot is taken even on a cancelled context; only waiting watches it.
	if err := g.acquire(cancelled); err != nil {
		t.Fatalf("acquire of a free slot: %v", err)
	}
	if err := g.acquire(cancelled); err != context.Canceled {
		t.Fatalf("acquire on a full gate = %v, want context.Canceled", err)
	}
	timed, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.acquire(timed); err != context.DeadlineExceeded {
		t.Fatalf("acquire on a full gate = %v, want context.DeadlineExceeded", err)
	}
	wantGate(t, g, 1, 1, 0)
}

func TestGate_Resize(t *testing.T) {
	g := newGate(1)
	if err := g.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := waiter(t, g)
	g.resize(3)
	admitted(t, w)
	wantGate(t, g, 2, 3, 1)

	// Shrinking below what is in use admits nobody until enough is released.
	g.resize(1)
	wantGate(t, g, 2, 1, 0)
	w = waiter(t, g)
	g.release()
	wantGate(t, g, 1, 1, 0)
	g.release()
	admitted(t, w)
	wantGate(t, g, 1, 1, 0)
}

func TestGate_Floors(t *testing.T) {
	for _, limit := range []int{0, -3} {
		wantGate(t, newGate(limit), 0, 1, 1)
	}
	g := newGate(4)
	g.resize(0)
	wantGate(t, g, 0, 1, 1)
	// A release with nothing held does not open an extra slot.
	g.release()
	wantGate(t, g, 0, 1, 1)
}

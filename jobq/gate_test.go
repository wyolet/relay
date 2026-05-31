package jobq

import (
	"context"
	"testing"
	"time"
)

func TestGate_BoundsConcurrency(t *testing.T) {
	g := newGate(2)
	ctx := context.Background()

	if err := g.acquire(ctx); err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := g.acquire(ctx); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if got := g.available(); got != 0 {
		t.Fatalf("available = %d, want 0", got)
	}

	// Third acquire must block until a slot frees.
	acquired := make(chan struct{})
	go func() {
		_ = g.acquire(ctx)
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("third acquire returned while gate full")
	case <-time.After(50 * time.Millisecond):
	}

	g.release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("third acquire did not proceed after release")
	}
}

func TestGate_AcquireRespectsContext(t *testing.T) {
	g := newGate(1)
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx); err == nil {
		t.Fatal("acquire on full gate returned nil; want ctx error")
	}
}

func TestGate_ResizeUpAdmitsWaiters(t *testing.T) {
	g := newGate(1)
	ctx := context.Background()
	if err := g.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	admitted := make(chan struct{})
	go func() {
		_ = g.acquire(ctx)
		close(admitted)
	}()
	select {
	case <-admitted:
		t.Fatal("waiter admitted before resize")
	case <-time.After(50 * time.Millisecond):
	}

	g.resize(2)
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("waiter not admitted after resize up")
	}
}

func TestGate_ResizeDownDrains(t *testing.T) {
	g := newGate(3)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := g.acquire(ctx); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	// Shrink below in-use: no new admits, in-flight keep running.
	g.resize(1)
	if got := g.available(); got != 0 {
		t.Fatalf("available after shrink = %d, want 0", got)
	}

	// Releasing two should still leave the gate full (inUse 1 == limit 1).
	g.release()
	g.release()
	if got := g.available(); got != 0 {
		t.Fatalf("available after draining to limit = %d, want 0", got)
	}
	// One more release opens exactly one slot.
	g.release()
	if got := g.available(); got != 1 {
		t.Fatalf("available = %d, want 1", got)
	}
}

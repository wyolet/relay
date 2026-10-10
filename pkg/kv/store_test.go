package kv

import (
	"context"
	"testing"
	"time"
)

// Behaviour every backend shares lives in contract_test.go; these reach Mem's
// internals.

func newStore(t *testing.T) *Mem {
	t.Helper()
	s := NewMem()
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIncrExpiredKeyCreatesPersistentCounter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	s.data.Store("counter", entry{value: []byte("41"), deadline: time.Now().Add(-time.Second)})

	got, err := s.Incr(ctx, "counter", 1)
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if got != 1 {
		t.Fatalf("Incr returned %d, want 1 from a fresh counter", got)
	}
	v, err := s.Get(ctx, "counter")
	if err != nil {
		t.Fatalf("Get after Incr: %v", err)
	}
	if string(v) != "1" {
		t.Fatalf("Get after Incr = %q, want %q", v, "1")
	}
}

func TestExpireZeroClearsExpiry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Set(ctx, "persist", []byte("v"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.Expire(ctx, "persist", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if _, err := s.Get(ctx, "persist"); err != nil {
		t.Fatalf("expected key to persist after Expire(0), got %v", err)
	}
}

func TestCloseStopsJanitor(t *testing.T) {
	s := NewMem()
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	// stopped channel is closed, confirming the janitor goroutine exited
	select {
	case <-s.stopped:
	default:
		t.Fatal("janitor did not stop after Close")
	}
}

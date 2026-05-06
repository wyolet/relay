package kv

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Mem {
	t.Helper()
	s := NewMem()
	t.Cleanup(func() { s.Close() })
	return s
}

func TestGetSetGet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	_, err := s.Get(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := s.Set(ctx, "k", []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "hello" {
		t.Fatalf("want hello, got %s", v)
	}
}

func TestIncrAtomic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Incr(ctx, "counter", 100); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	v, err := s.Get(ctx, "counter")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "100000" {
		t.Fatalf("want 100000, got %s", v)
	}
}

func TestTTLEviction(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Set(ctx, "ttl-key", []byte("val"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "ttl-key"); err != nil {
		t.Fatalf("expected value before expiry, got %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if _, err := s.Get(ctx, "ttl-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after expiry, got %v", err)
	}

	entries, err := s.Range(ctx, "ttl-")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries after expiry, got %d", len(entries))
	}
}

func TestRangePrefix(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	keys := []string{"a:1", "a:2", "b:1"}
	for _, k := range keys {
		if err := s.Set(ctx, k, []byte(k+"-val"), 0); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.Range(ctx, "a:")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Key != "a:1" || entries[1].Key != "a:2" {
		t.Fatalf("unexpected order: %v", entries)
	}
}

func TestWithLockSerial(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	var (
		mu      sync.Mutex
		counter int
		interleaved bool
	)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.WithLock(ctx, []string{"x", "y"}, func(ctx context.Context) error {
				mu.Lock()
				counter++
				snap := counter
				mu.Unlock()

				time.Sleep(20 * time.Millisecond)

				mu.Lock()
				if counter != snap {
					interleaved = true
				}
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if interleaved {
		t.Fatal("detected interleaving inside WithLock")
	}
}

func TestWithLockDeadlockFree(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = s.WithLock(ctx, []string{"a", "b"}, func(ctx context.Context) error {
			time.Sleep(5 * time.Millisecond)
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_ = s.WithLock(ctx, []string{"b", "a"}, func(ctx context.Context) error {
			time.Sleep(5 * time.Millisecond)
			return nil
		})
	}()
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock detected: WithLock did not complete")
	}
}

func TestExpireResetsTTL(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if err := s.Set(ctx, "exp-key", []byte("v"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	if err := s.Expire(ctx, "exp-key", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	if _, err := s.Get(ctx, "exp-key"); err != nil {
		t.Fatalf("expected key present after TTL reset, got %v", err)
	}

	if err := s.Expire(ctx, "no-such-key", 100*time.Millisecond); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
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

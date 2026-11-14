package kv

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
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

// Deadlines on a fake clock: hash fields, ExpireNX and the janitor.
func TestMemDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := NewMem()
		defer s.Close()
		get := func(read func() ([]byte, error)) string {
			v, err := read()
			if errors.Is(err, ErrNotFound) {
				return "<gone>"
			}
			return string(v)
		}
		field := func(name string) string {
			return get(func() ([]byte, error) { return s.HGet(ctx, "h", name) })
		}

		// A field written without a ttl keeps the deadline it has, or gets none.
		_ = s.HSet(ctx, "h", "timed", []byte("1"), time.Second)
		_ = s.HSet(ctx, "h", "timed", []byte("2"), 0)
		_ = s.HSet(ctx, "h", "plain", []byte("3"), 0)
		// ExpireNX gives a deadline only to a live key that has none.
		_ = s.Set(ctx, "nx", []byte("v"), 0)
		for i, want := range []bool{true, false} {
			if set, err := s.ExpireNX(ctx, "nx", time.Second); set != want || err != nil {
				t.Fatalf("ExpireNX call %d = %v, %v; want %v", i+1, set, err, want)
			}
		}
		if set, err := s.ExpireNX(ctx, "missing", time.Second); set || err != nil {
			t.Fatalf("ExpireNX on a missing key = %v, %v", set, err)
		}
		_ = s.Set(ctx, "swept", []byte("v"), time.Second)

		time.Sleep(time.Second) // exactly at the deadline: still there
		if got := [3]string{field("timed"), field("plain"), get(func() ([]byte, error) { return s.Get(ctx, "nx") })}; got != [3]string{"2", "3", "v"} {
			t.Fatalf("at the deadline: timed, plain, nx = %v", got)
		}
		time.Sleep(time.Millisecond)
		if got := [3]string{field("timed"), field("plain"), get(func() ([]byte, error) { return s.Get(ctx, "nx") })}; got != [3]string{"<gone>", "3", "<gone>"} {
			t.Fatalf("past the deadline: timed, plain, nx = %v", got)
		}
		if set, err := s.ExpireNX(ctx, "nx", time.Second); set || err != nil {
			t.Fatalf("ExpireNX on an expired key = %v, %v", set, err)
		}

		// The janitor removes an expired key nobody reads.
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if _, ok := s.data.Load("swept"); ok {
			t.Fatal("the janitor left an expired key in the map")
		}
	})
}

func TestMemRunScriptUnregistered(t *testing.T) {
	s := newStore(t)
	const want = `state: script "nope" not registered`
	if _, err := s.RunScript(context.Background(), "nope", "return 1", nil); err == nil || err.Error() != want {
		t.Fatalf("RunScript: err = %v, want %q", err, want)
	}
	results := s.RunScriptBatch(context.Background(), []ScriptCall{{Name: "nope"}})
	if len(results) != 1 || results[0].Err == nil || results[0].Err.Error() != want {
		t.Fatalf("RunScriptBatch = %+v, want one result with %q", results, want)
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

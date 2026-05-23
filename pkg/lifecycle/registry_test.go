package lifecycle

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- registration ---

func TestRegisterNilSkipped(t *testing.T) {
	r := New()
	r.RegisterPreFlight(nil)
	r.RegisterPostFlight(nil)
	if r.PreFlightCount() != 0 || r.PostFlightCount() != 0 {
		t.Fatalf("nil hooks should not be stored; counts: pre=%d post=%d",
			r.PreFlightCount(), r.PostFlightCount())
	}
}

func TestEmptyRegistryNoPanic(t *testing.T) {
	r := New()
	lc := NewContext("req-1", "test", time.Now())
	if err := r.RunPreFlight(context.Background(), lc, &PreFlightEvent{}); err != nil {
		t.Fatalf("empty pre-flight: %v", err)
	}
	r.FirePostFlight(context.Background(), lc, &PostFlightEvent{})
}

// --- pre-flight semantics ---

func TestPreFlightSequentialOrder(t *testing.T) {
	r := New()
	var order []string
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		order = append(order, "a")
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		order = append(order, "b")
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		order = append(order, "c")
		return nil
	})

	if err := r.RunPreFlight(context.Background(), NewContext("r", "test", time.Now()), &PreFlightEvent{}); err != nil {
		t.Fatalf("pre-flight: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !equalStrings(order, want) {
		t.Fatalf("order: got %v want %v", order, want)
	}
}

func TestPreFlightAbortOnError(t *testing.T) {
	r := New()
	abort := errors.New("budget exceeded")
	var ran []string
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		ran = append(ran, "a")
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		ran = append(ran, "b")
		return abort
	})
	r.RegisterPreFlight(func(_ context.Context, _ *Context, _ *PreFlightEvent) error {
		ran = append(ran, "c") // must not run
		return nil
	})

	err := r.RunPreFlight(context.Background(), NewContext("r", "test", time.Now()), &PreFlightEvent{})
	if !errors.Is(err, abort) {
		t.Fatalf("expected abort error to propagate, got %v", err)
	}
	if !equalStrings(ran, []string{"a", "b"}) {
		t.Fatalf("expected [a b], got %v", ran)
	}
}

func TestPreFlightMutatesContext(t *testing.T) {
	r := New()
	r.RegisterPreFlight(func(_ context.Context, lc *Context, _ *PreFlightEvent) error {
		lc.PolicyID = "pol-123"
		lc.Metadata["auth_user"] = "alice"
		return nil
	})
	r.RegisterPreFlight(func(_ context.Context, lc *Context, _ *PreFlightEvent) error {
		// Subsequent middleware sees prior mutations.
		if lc.PolicyID != "pol-123" {
			t.Errorf("expected PolicyID set by prior middleware, got %q", lc.PolicyID)
		}
		if lc.Metadata["auth_user"] != "alice" {
			t.Errorf("expected metadata from prior middleware, got %v", lc.Metadata)
		}
		return nil
	})

	lc := NewContext("r", "test", time.Now())
	if err := r.RunPreFlight(context.Background(), lc, &PreFlightEvent{}); err != nil {
		t.Fatalf("pre-flight: %v", err)
	}
}

// --- post-flight semantics ---

func TestPostFlightAllHooksRun(t *testing.T) {
	r := New()
	var ran int64
	for i := 0; i < 5; i++ {
		r.RegisterPostFlight(func(_ context.Context, _ *Context, _ *PostFlightEvent) {
			atomic.AddInt64(&ran, 1)
		})
	}
	r.FirePostFlight(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if got := atomic.LoadInt64(&ran); got != 5 {
		t.Fatalf("expected 5 hooks ran, got %d", got)
	}
}

func TestPostFlightParallel(t *testing.T) {
	// Each hook sleeps for D; if they run sequentially total > 5*D,
	// parallel total ≈ D. Use a generous margin to avoid CI flake.
	r := New()
	const hooks = 5
	const sleep = 50 * time.Millisecond
	for i := 0; i < hooks; i++ {
		r.RegisterPostFlight(func(_ context.Context, _ *Context, _ *PostFlightEvent) {
			time.Sleep(sleep)
		})
	}
	start := time.Now()
	r.FirePostFlight(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	elapsed := time.Since(start)
	// Sequential would be ~hooks*sleep; parallel ~sleep. Allow up to 3*sleep
	// for scheduling jitter.
	if elapsed > 3*sleep {
		t.Fatalf("post-flight not parallel: elapsed=%v, sequential≈%v, parallel≈%v",
			elapsed, hooks*sleep, sleep)
	}
}

func TestPostFlightPanicIsolated(t *testing.T) {
	r := New()
	var ran int64
	r.RegisterPostFlight(func(_ context.Context, _ *Context, _ *PostFlightEvent) {
		atomic.AddInt64(&ran, 1)
	})
	r.RegisterPostFlight(func(_ context.Context, _ *Context, _ *PostFlightEvent) {
		panic("intentional in test")
	})
	r.RegisterPostFlight(func(_ context.Context, _ *Context, _ *PostFlightEvent) {
		atomic.AddInt64(&ran, 1)
	})

	// Must not panic out of FirePostFlight; both non-panicking hooks must run.
	r.FirePostFlight(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
	if got := atomic.LoadInt64(&ran); got != 2 {
		t.Fatalf("expected 2 non-panicking hooks to run, got %d", got)
	}
}

func TestPostFlightSharesContextAndEvent(t *testing.T) {
	r := New()
	wantID := "req-shared"
	wantStatus := 200

	var seenIDs, seenStatus int64
	for i := 0; i < 3; i++ {
		r.RegisterPostFlight(func(_ context.Context, lc *Context, ev *PostFlightEvent) {
			if lc.RequestID == wantID {
				atomic.AddInt64(&seenIDs, 1)
			}
			if ev.Status == wantStatus {
				atomic.AddInt64(&seenStatus, 1)
			}
		})
	}

	lc := NewContext(wantID, "test", time.Now())
	r.FirePostFlight(context.Background(), lc, &PostFlightEvent{Status: wantStatus})

	if seenIDs != 3 || seenStatus != 3 {
		t.Fatalf("hooks did not see shared lc/ev: ids=%d status=%d", seenIDs, seenStatus)
	}
}

// --- concurrent register / fire ---

func TestConcurrentRegisterAndFire(t *testing.T) {
	// race detector catches data races on the slices
	r := New()
	var fired int64

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RegisterPostFlight(func(_ context.Context, _ *Context, _ *PostFlightEvent) {
				atomic.AddInt64(&fired, 1)
			})
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.FirePostFlight(context.Background(), NewContext("r", "test", time.Now()), &PostFlightEvent{})
		}()
	}
	wg.Wait()

	if r.PostFlightCount() != 50 {
		t.Fatalf("expected 50 registered, got %d", r.PostFlightCount())
	}
}

// --- helpers ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

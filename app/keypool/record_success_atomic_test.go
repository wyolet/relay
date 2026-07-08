package keypool

import (
	"context"
	"sync"
	"testing"
)

// TestRecordSuccess_ReadsPriorState confirms the atomic Lua path returns the
// prior record (used for the from-state log line): after a failure opens the
// breaker, RecordSuccess observes the open state and writes closed in one round
// trip.
func TestRecordSuccess_ReadsPriorState(t *testing.T) {
	sel, ms := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-prior"

	// Confirm the atomic runner is wired (Mem implements Scripter).
	if sel.runner == nil {
		t.Fatal("expected a script runner on a Mem-backed Selector")
	}
	_ = ms

	sel.RecordFailure(ctx, k, FailureAuth, 0)
	if rec := sel.readRecord(ctx, k); rec.State != CircuitOpen {
		t.Fatalf("precondition: want open, got %v", rec.State)
	}

	sel.RecordSuccess(ctx, k)
	rec := sel.readRecord(ctx, k)
	if rec.State != CircuitClosed || rec.BackoffStep != 0 || rec.Reason != "" {
		t.Fatalf("after success: want closed/0/'' got %v/%d/%q", rec.State, rec.BackoffStep, rec.Reason)
	}
}

// TestRecordSuccess_ConcurrentNoTear: RecordSuccess and RecordFailure hammering
// the same key concurrently must never leave a torn/undecodable record — the
// server-side GET+SET is atomic, so every observed record is a clean whole.
// Run under -race. The final state is whichever writer landed last; we only
// assert it decodes and is a valid state.
func TestRecordSuccess_ConcurrentNoTear(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-race"

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			sel.RecordSuccess(ctx, k)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			sel.RecordFailure(ctx, k, FailureServerError, 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			// Concurrent reads must always decode a whole record.
			rec, _ := sel.ReadCircuit(ctx, k)
			switch rec.State {
			case CircuitClosed, CircuitOpen, CircuitHalfOpen:
			default:
				t.Errorf("read a torn/unknown state: %v", rec.State)
			}
		}
	}()
	wg.Wait()

	rec, found := sel.ReadCircuit(ctx, k)
	if !found {
		t.Fatal("expected a record after concurrent writes")
	}
	switch rec.State {
	case CircuitClosed, CircuitOpen, CircuitHalfOpen:
	default:
		t.Fatalf("final state undecodable: %v", rec.State)
	}
}

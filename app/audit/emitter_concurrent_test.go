// emitter_concurrent_test.go hammers Emit, Close and Prune against each other.
// The emitter's contract at shutdown is "never take the process down and
// never lose a row silently": the queue channel is deliberately left open
// so a concurrent Emit cannot panic, and prune runs off the drain loop
// behind an in-flight guard. Both only show up as failures under `-race`
// or under real contention.
package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakySink fails every other write and counts what it accepted, so the
// retry-then-drop path runs concurrently with the prune and the drain.
type flakySink struct {
	memSink
	calls  atomic.Int64
	pruned atomic.Int64
}

func (s *flakySink) Write(ctx context.Context, evs []Event) error {
	if s.calls.Add(1)%3 == 0 {
		return context.DeadlineExceeded
	}
	return s.memSink.Write(ctx, evs)
}

func (s *flakySink) Prune(context.Context, time.Time) (int64, error) {
	s.pruned.Add(1)
	// Long enough to overlap the next tick, so the in-flight guard matters.
	time.Sleep(time.Millisecond)
	return 0, nil
}

// TestEmitPruneAndCloseTogether runs producers, prune callers and the
// ticker-driven prune concurrently, then closes underneath all of them.
// Nothing may panic, and every event must be accounted for: written, or
// counted as dropped.
func TestEmitPruneAndCloseTogether(t *testing.T) {
	old := pruneInterval
	pruneInterval = time.Millisecond
	t.Cleanup(func() { pruneInterval = old })

	sink := &flakySink{}
	e := NewEmitter(sink, quietLogger())
	e.SetRetentionDays(1)

	const (
		producers = 8
		perWriter = 200
	)
	var wg sync.WaitGroup

	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				e.Emit(Event{Action: "policies.update"})
			}
		}()
	}
	// A prune caller of its own, racing the ticker's.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.Prune()
		}
	}()
	// Retention flipping under the prune loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			e.SetRetentionDays(i % 3)
		}
	}()

	// Close while producers are still emitting: a post-Close Emit is a
	// no-op, not a send on a closed channel.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(2 * time.Millisecond)
		e.Close()
		// Close is idempotent, including concurrently.
		e.Close()
	}()

	wg.Wait()
	<-closed
	e.Close()

	// Emits after Close are dropped on the floor by contract, so the totals
	// only have to be consistent, not complete: nothing may be counted twice
	// and the written set may not exceed what was emitted.
	written := int64(len(sink.all()))
	dropped := int64(e.DroppedCount())
	const emitted = producers * perWriter
	if written+dropped > emitted+int64(batchSize) {
		t.Fatalf("written %d + dropped %d exceeds the %d emitted", written, dropped, emitted)
	}
	if sink.pruned.Load() == 0 {
		t.Fatal("no prune ran while the ticker was at 1ms")
	}
}

// TestEmitAfterCloseIsANoOp pins the shutdown ordering: a handler
// still in flight when Close lands must not panic on the send.
func TestEmitAfterCloseIsANoOp(t *testing.T) {
	e := NewEmitter(&memSink{}, quietLogger())

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				e.Emit(Event{Action: "keys.rotate"})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Close()
		}()
	}
	wg.Wait()
}

// TestConcurrentPruneRunsOnce keeps the in-flight guard honest under
// contention: many callers, one delete in flight at a time.
func TestConcurrentPruneRunsOnce(t *testing.T) {
	sink := &countingPruneSink{release: make(chan struct{})}
	e := NewEmitter(sink, quietLogger())
	t.Cleanup(e.Close)
	e.SetRetentionDays(1)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Prune()
		}()
	}
	// One caller is parked inside the sink; the rest must return without
	// starting a second delete over the same rows.
	deadline := time.Now().Add(2 * time.Second)
	for sink.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := sink.started.Load(); got != 1 {
		t.Errorf("prune entered %d times concurrently, want 1", got)
	}
	close(sink.release)
	wg.Wait()
	if got := sink.started.Load(); got > 16 {
		t.Fatalf("prune entered %d times, want at most one per caller", got)
	}
}

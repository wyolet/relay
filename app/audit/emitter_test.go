package audit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingSink always errors, and counts how many times Write was called so
// a test can assert the retry happened.
type failingSink struct {
	memSink
	writes atomic.Int32
}

func (f *failingSink) Write(ctx context.Context, evs []Event) error {
	f.writes.Add(1)
	return errors.New("sink unavailable")
}

// blockingSink parks the drain goroutine so the queue can be filled past
// its bound deterministically.
type blockingSink struct {
	memSink
	release chan struct{}
}

func (b *blockingSink) Write(ctx context.Context, evs []Event) error {
	<-b.release
	return b.memSink.Write(ctx, evs)
}

func TestEmitterDropsOnFullQueue(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	e := NewEmitter(sink, quietLogger())

	// The drain goroutine buffers a full batch before it blocks on the
	// sink; past that the bounded queue fills and everything else drops.
	for i := 0; i < QueueSize+batchSize+50; i++ {
		e.Emit(Event{Action: "policies.update"})
	}
	if got := e.DroppedCount(); got == 0 {
		t.Fatal("dropped = 0, want > 0 once the queue is full")
	}
	close(sink.release)
	e.Close()
}

func TestEmitterFlushesOnBatchSize(t *testing.T) {
	sink := &memSink{}
	e := NewEmitter(sink, quietLogger())
	for i := 0; i < batchSize; i++ {
		e.Emit(Event{Action: "policies.update"})
	}
	// The size-triggered flush lands well before the 1s tick.
	deadline := time.Now().Add(2 * time.Second)
	for len(sink.all()) < batchSize && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(sink.all()); got != batchSize {
		t.Fatalf("written = %d before the tick, want %d from the size-triggered flush", got, batchSize)
	}
	e.Close()
}

func TestEmitterFlushesOnTick(t *testing.T) {
	sink := &memSink{}
	e := NewEmitter(sink, quietLogger())
	e.Emit(Event{Action: "policies.update"})
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(sink.all()); got != 1 {
		t.Fatalf("written = %d, want 1 flushed by the interval tick", got)
	}
	e.Close()
}

func TestEmitterCloseDrains(t *testing.T) {
	sink := &memSink{}
	e := NewEmitter(sink, quietLogger())
	for i := 0; i < 10; i++ {
		e.Emit(Event{Action: "policies.delete"})
	}
	e.Close()
	if got := len(sink.all()); got != 10 {
		t.Fatalf("written = %d after Close, want 10", got)
	}
	// Emit after Close is a no-op, and Close is idempotent.
	e.Emit(Event{Action: "policies.delete"})
	e.Close()
	if got := len(sink.all()); got != 10 {
		t.Fatalf("written = %d after a post-Close Emit, want 10", got)
	}
}

// A sink that keeps failing must not lose the row silently: the write is
// retried once, and only then counted as dropped.
func TestEmitterRetriesThenDropsOnPersistentSinkFailure(t *testing.T) {
	sink := &failingSink{}
	e := NewEmitter(sink, quietLogger())
	const n = 10
	for i := 0; i < n; i++ {
		e.Emit(Event{Action: "policies.update"})
	}
	e.Close()

	if got := e.DroppedCount(); got != n {
		t.Fatalf("dropped = %d, want %d", got, n)
	}
	if got := sink.writes.Load(); got != 2 {
		t.Fatalf("sink.Write calls = %d, want 2 (the write + one retry)", got)
	}
}

// Close no longer closes the queue channel, so a concurrent Emit is a no-op
// rather than a send-on-closed-channel panic — run under -race.
func TestEmitterCloseDoesNotRaceEmit(t *testing.T) {
	sink := &memSink{}
	e := NewEmitter(sink, quietLogger())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.Emit(Event{Action: "policies.update"})
			}
		}()
	}
	e.Close()
	wg.Wait()

	// Already-queued events before Close still get written; nothing panics.
	e.Emit(Event{Action: "policies.update"})
	e.Close()
}

func TestPruneUsesLiveRetention(t *testing.T) {
	sink := &memSink{}
	e := NewEmitter(sink, quietLogger())
	defer e.Close()

	// Retention unset: nothing is pruned.
	e.Prune()
	sink.mu.Lock()
	n := sink.pruned
	sink.mu.Unlock()
	if n != 0 {
		t.Fatalf("prune calls = %d with retention unset, want 0", n)
	}

	e.SetRetentionDays(30)
	e.Prune()
	sink.mu.Lock()
	first := sink.before
	sink.mu.Unlock()
	if d := time.Since(first).Hours() / 24; d < 29 || d > 31 {
		t.Fatalf("prune cutoff %v is %.1f days old, want ~30", first, d)
	}

	// A live settings change moves the cutoff on the next pass.
	e.SetRetentionDays(1)
	e.Prune()
	sink.mu.Lock()
	second := sink.before
	sink.mu.Unlock()
	if !second.After(first) {
		t.Fatalf("cutoff %v did not move after shortening retention (was %v)", second, first)
	}
	if d := time.Since(second).Hours() / 24; d < 0.5 || d > 1.5 {
		t.Fatalf("prune cutoff %v is %.2f days old, want ~1", second, d)
	}
}

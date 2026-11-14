package audit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingSink fails its first failures writes, and counts every call so a
// test can assert the retry happened.
type failingSink struct {
	memSink
	failures int32
	writes   atomic.Int32
}

func (f *failingSink) Write(ctx context.Context, evs []Event) error {
	if f.writes.Add(1) <= f.failures {
		return errors.New("sink unavailable")
	}
	return f.memSink.Write(ctx, evs)
}

// A failed write is retried once: a blip loses nothing, and only a sink that
// fails the retry too has its rows counted as dropped.
func TestEmitterRetriesAFailedWriteOnce(t *testing.T) {
	const n = 10
	for _, tc := range []struct {
		name                     string
		failures                 int32
		wantWritten, wantDropped int
	}{
		{"sink blip", 1, n, 0},
		{"sink down", 2, 0, n},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &failingSink{failures: tc.failures}
			e := NewEmitter(sink, quietLogger())
			for range n {
				e.Emit(Event{Action: "policies.update"})
			}
			e.Close()
			if got := len(sink.all()); got != tc.wantWritten || e.DroppedCount() != uint64(tc.wantDropped) {
				t.Fatalf("written = %d, dropped = %d; want %d and %d", got, e.DroppedCount(), tc.wantWritten, tc.wantDropped)
			}
			if got := sink.writes.Load(); got != 2 {
				t.Fatalf("sink.Write calls = %d, want 2 (the write + one retry)", got)
			}
		})
	}
}

// blockingSink parks the drain goroutine in its first Write, so the queue
// can be filled past its bound deterministically.
type blockingSink struct {
	memSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSink) Write(ctx context.Context, evs []Event) error {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.memSink.Write(ctx, evs)
}

// With the sink stalled on a full batch the queue takes QueueSize more
// events; every one past that is dropped and counted, and what was queued
// still lands, in order, once the sink recovers.
func TestEmitterDropsOnFullQueue(t *testing.T) {
	const overflow = 50
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	e := NewEmitter(sink, quietLogger())
	emit := func(from, n int) {
		for i := from; i < from+n; i++ {
			e.Emit(Event{Action: "policies.update", Resource: Resource{ID: strconv.Itoa(i)}})
		}
	}
	emit(0, batchSize)
	<-sink.entered
	emit(batchSize, QueueSize+overflow)
	if got, depth := e.DroppedCount(), e.QueueDepth(); got != overflow || depth != QueueSize {
		t.Fatalf("dropped = %d, queue depth = %d; want %d and %d", got, depth, overflow, QueueSize)
	}

	close(sink.release)
	e.Close()
	written := sink.all()
	if len(written) != batchSize+QueueSize {
		t.Fatalf("written = %d, want the %d that were queued", len(written), batchSize+QueueSize)
	}
	for i, ev := range written {
		if ev.Resource.ID != strconv.Itoa(i) {
			t.Fatalf("event %d is %q: queued events were written out of order", i, ev.Resource.ID)
		}
	}
	if e.DroppedCount() != overflow || e.QueueDepth() != 0 {
		t.Fatalf("after Close: dropped = %d, depth = %d", e.DroppedCount(), e.QueueDepth())
	}
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

// Close leaves the queue channel open, so an Emit still in flight when Close
// lands — from a handler racing shutdown, or several Closes at once — is a
// no-op rather than a send-on-closed-channel panic. Run under -race.
func TestEmitterCloseDoesNotRaceEmit(t *testing.T) {
	sink := &memSink{}
	e := NewEmitter(sink, quietLogger())

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				e.Emit(Event{Action: "policies.update"})
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

	// After Close, Emit and Close stay no-ops.
	e.Emit(Event{Action: "policies.update"})
	e.Close()
}

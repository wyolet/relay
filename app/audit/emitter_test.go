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
	// Retention flipping under the prune loop, through the disabled value 0.
	// Starting at 1 keeps the first value positive: a flipper that sets 0 before
	// the direct prune loop runs could otherwise starve every prune.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 500; i++ {
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

// countingPruneSink blocks in Prune until released, counting entries.
type countingPruneSink struct {
	memSink
	started atomic.Int64
	release chan struct{}
}

func (s *countingPruneSink) Prune(context.Context, time.Time) (int64, error) {
	s.started.Add(1)
	<-s.release
	return 0, nil
}

// slowPruneSink parks Prune until released, so a test can hold one in
// flight while events keep arriving.
type slowPruneSink struct {
	memSink
	entered chan struct{}
	release chan struct{}
}

func (s *slowPruneSink) Prune(ctx context.Context, before time.Time) (int64, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return 0, nil
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

// A prune over a large table outlives its own interval, and the ticker keeps
// firing. While one prune is in flight every other caller returns at once
// instead of running the same delete over the same rows; once it finishes,
// pruning resumes.
func TestPruneRunsOneAtATime(t *testing.T) {
	sink := &countingPruneSink{release: make(chan struct{})}
	e := NewEmitter(sink, quietLogger())
	t.Cleanup(e.Close)
	e.SetRetentionDays(1)

	parked := make(chan struct{})
	go func() {
		defer close(parked)
		e.Prune()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for sink.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Prune()
		}()
	}
	returned := make(chan struct{})
	go func() { wg.Wait(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("a Prune call blocked behind the one in flight")
	}
	if got := sink.started.Load(); got != 1 {
		t.Fatalf("prune entered %d times concurrently, want 1", got)
	}

	close(sink.release)
	<-parked

	sink.release = make(chan struct{})
	close(sink.release)
	e.Prune()
	if got := sink.started.Load(); got != 2 {
		t.Fatalf("prune ran %d times in total, want 2", got)
	}
}

// A prune against a large table takes seconds. Running it on the drain loop
// stops the queue draining for that long and the emitter starts dropping
// rows — which is the one thing an audit log may not do.
func TestPruneDoesNotStallTheDrain(t *testing.T) {
	old := pruneInterval
	pruneInterval = 5 * time.Millisecond
	t.Cleanup(func() { pruneInterval = old })

	sink := &slowPruneSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	e := NewEmitter(sink, quietLogger())
	e.SetRetentionDays(1)

	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("prune never ran")
	}

	// The prune is parked. Events emitted now must still reach the sink.
	for i := 0; i < batchSize; i++ {
		e.Emit(Event{Action: "policies.update"})
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(sink.all()) < batchSize && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := len(sink.all())
	close(sink.release)
	e.Close()

	if got < batchSize {
		t.Fatalf("sink received %d of %d events while a prune was in flight — the drain stalled", got, batchSize)
	}
	if e.DroppedCount() != 0 {
		t.Fatalf("dropped %d events during a prune", e.DroppedCount())
	}
}

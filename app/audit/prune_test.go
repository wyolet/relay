package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

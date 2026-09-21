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

// A prune over a large table outlives its own interval, and the ticker keeps
// firing. TestPruneSkipsWhileOneIsInFlight pins that a second prune returns
// straight away instead of running the same delete over the same rows.
func TestPruneSkipsWhileOneIsInFlight(t *testing.T) {
	sink := &countingPruneSink{release: make(chan struct{})}
	e := NewEmitter(sink, quietLogger())
	t.Cleanup(e.Close)
	e.SetRetentionDays(1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.Prune()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for sink.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	e.Prune() // must not block, and must not start a second delete
	if got := sink.started.Load(); got != 1 {
		t.Fatalf("prune ran %d times concurrently, want 1", got)
	}

	close(sink.release)
	wg.Wait()

	// Once the first finishes, pruning resumes.
	sink.release = make(chan struct{})
	close(sink.release)
	e.Prune()
	if got := sink.started.Load(); got != 2 {
		t.Fatalf("prune ran %d times in total, want 2", got)
	}
}

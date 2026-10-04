package audit

import (
	"context"
	"testing"
	"time"
)

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

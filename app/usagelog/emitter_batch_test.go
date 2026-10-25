package usagelog

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// batchSink records each batch it receives. The first call signals entered
// and waits on release, so a test can park the drain goroutine and queue a
// burst behind it.
type batchSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	sizes   []int
	written []string
}

func newBatchSink() *batchSink {
	return &batchSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *batchSink) park() {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
}

func (s *batchSink) Write(ev Event) error {
	return s.WriteBatch([]Event{ev})
}

func (s *batchSink) WriteBatch(evs []Event) error {
	s.park()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, len(evs))
	for _, ev := range evs {
		s.written = append(s.written, ev.RequestID)
	}
	return nil
}

func (s *batchSink) result() ([]int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sizes), slices.Clone(s.written)
}

// writeOnlySink has no WriteBatch, so the emitter must fall back to Write.
type writeOnlySink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	written []string
}

func (s *writeOnlySink) Write(ev Event) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, ev.RequestID)
	return nil
}

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func TestEmitter_DrainsBurstInBatches(t *testing.T) {
	sink := newBatchSink()
	e := NewEmitter(EmitterOptions{QueueSize: 2000, Logger: testLogger()}, sink)

	e.Emit(served("first"))
	<-sink.entered
	burst := ids("b", 1200)
	for _, id := range burst {
		e.Emit(served(id))
	}
	if d := e.QueueDepth(); d != len(burst) {
		t.Fatalf("queue depth with drain parked = %d, want %d", d, len(burst))
	}
	close(sink.release)
	e.Close()

	sizes, written := sink.result()
	if want := append([]string{"first"}, burst...); !slices.Equal(written, want) {
		t.Fatalf("written %d events out of order or incomplete, want %d", len(written), len(want))
	}
	if sizes[0] != 1 {
		t.Fatalf("first batch = %d, want 1 (the event that parked the drain)", sizes[0])
	}
	// 1200 queued events drain as 512 + 512 + 176.
	if !slices.Equal(sizes[1:], []int{drainBatchMax, drainBatchMax, 1200 - 2*drainBatchMax}) {
		t.Fatalf("batch sizes after the burst = %v, want full batches capped at %d", sizes[1:], drainBatchMax)
	}
	if e.QueueDepth() != 0 {
		t.Fatalf("queue depth after Close = %d, want 0", e.QueueDepth())
	}
}

func TestEmitter_FallsBackToWriteForNonBatchSink(t *testing.T) {
	sink := &writeOnlySink{entered: make(chan struct{}), release: make(chan struct{})}
	e := NewEmitter(EmitterOptions{QueueSize: 100, Logger: testLogger()}, sink)

	e.Emit(served("first"))
	<-sink.entered
	burst := ids("w", 50)
	for _, id := range burst {
		e.Emit(served(id))
	}
	close(sink.release)
	e.Close()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if want := append([]string{"first"}, burst...); !slices.Equal(sink.written, want) {
		t.Fatalf("Write-only sink got %v, want %v", sink.written, want)
	}
}

// Close must hand everything still queued to the sink before closing it.
func TestEmitter_CloseDrainsQueuedBatches(t *testing.T) {
	sink := newBatchSink()
	e := NewEmitter(EmitterOptions{QueueSize: 2000, Logger: testLogger()}, sink)

	e.Emit(served("first"))
	<-sink.entered
	queued := ids("q", 1500)
	for _, id := range queued {
		e.Emit(served(id))
	}
	closed := make(chan struct{})
	go func() {
		e.Close()
		close(closed)
	}()
	close(sink.release)
	<-closed

	if _, written := sink.result(); len(written) != 1+len(queued) {
		t.Fatalf("written after Close = %d, want %d", len(written), 1+len(queued))
	}
}

type failingSink struct{ calls int }

func (s *failingSink) Write(Event) error {
	s.calls++
	if s.calls%2 == 0 {
		return errors.New("refused")
	}
	return nil
}

func TestReloadableSink_WriteBatch(t *testing.T) {
	batch := []Event{{RequestID: "a"}, {RequestID: "b"}, {RequestID: "c"}}

	t.Run("delegates to a batch sink", func(t *testing.T) {
		inner := newBatchSink()
		close(inner.release)
		r := &reloadableSink{cur: inner}
		if err := r.WriteBatch(batch); err != nil {
			t.Fatal(err)
		}
		if sizes, _ := inner.result(); !slices.Equal(sizes, []int{3}) {
			t.Fatalf("inner batch sizes = %v, want [3]", sizes)
		}
	})

	t.Run("loops Write and reports failures", func(t *testing.T) {
		inner := &failingSink{}
		r := &reloadableSink{cur: inner}
		err := r.WriteBatch(batch)
		if inner.calls != 3 {
			t.Fatalf("Write calls = %d, want 3", inner.calls)
		}
		if err == nil || !strings.Contains(err.Error(), "1 of 3") {
			t.Fatalf("err = %v, want one of three events reported failed", err)
		}
	})

	t.Run("no sink installed", func(t *testing.T) {
		if err := (&reloadableSink{}).WriteBatch(batch); err != nil {
			t.Fatal(err)
		}
	})
}

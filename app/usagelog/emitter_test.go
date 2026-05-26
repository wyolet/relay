package usagelog

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestSink is a minimal JSONL sink for Emitter tests. The concrete
// file backend now lives in pkg/usage/file; the Emitter only needs the
// Sink interface, so tests carry their own tiny writer.
func newTestSink(w io.Writer) *testSink {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &testSink{enc: enc}
}

type testSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (s *testSink) Write(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(ev)
}

func TestEmitter_WritesEventToSink(t *testing.T) {
	var buf bytes.Buffer
	sink := newTestSink(&buf)
	e := NewEmitter(EmitterOptions{}, sink)
	defer e.Close()

	e.Emit(Event{RequestID: "r1", Source: "pipeline", Status: 200, DurationMs: 42})
	e.Close()

	// Decoder should read one JSON value per line.
	var got Event
	dec := json.NewDecoder(strings.NewReader(buf.String()))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequestID != "r1" || got.DurationMs != 42 {
		t.Fatalf("got %+v", got)
	}
}

func TestEmitter_DropOnFull(t *testing.T) {
	// Tiny queue + a blocking sink so all subsequent emits drop.
	block := make(chan struct{})
	sink := &blockingSink{ch: block}
	e := NewEmitter(EmitterOptions{QueueSize: 1}, sink)
	// Drain goroutine is blocked on the sink; unblock it BEFORE
	// Close (defers run LIFO so order matters).
	t.Cleanup(func() {
		close(block)
		e.Close()
	})

	// Fill the queue (1 item drains into the goroutine, the next
	// fills the channel slot, anything beyond should drop).
	for i := 0; i < 50; i++ {
		e.Emit(Event{RequestID: "r"})
	}
	// Allow time for the drain goroutine to consume what it can.
	time.Sleep(50 * time.Millisecond)
	if e.Dropped() == 0 {
		t.Fatal("expected non-zero drops with a blocking sink and tiny queue")
	}
}

func TestEmitter_ConcurrentEmit(t *testing.T) {
	var buf safeBuffer
	sink := newTestSink(&buf)
	e := NewEmitter(EmitterOptions{QueueSize: 10_000}, sink)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				e.Emit(Event{RequestID: "concurrent"})
			}
		}()
	}
	wg.Wait()
	e.Close()

	// Count lines. 100 * 10 = 1000 events; some might drop under
	// extreme contention but with a 10k buffer it should be all.
	lines := strings.Count(buf.String(), "\n")
	if lines+int(e.Dropped()) != 1000 {
		t.Fatalf("written=%d dropped=%d total=%d, want 1000",
			lines, e.Dropped(), lines+int(e.Dropped()))
	}
}

func TestEmitter_CloseIsIdempotent(t *testing.T) {
	e := NewEmitter(EmitterOptions{}, newTestSink(&bytes.Buffer{}))
	e.Close()
	e.Close() // must not panic on double-close
}

// --- helpers ---

type blockingSink struct{ ch <-chan struct{} }

func (b *blockingSink) Write(_ Event) error {
	<-b.ch
	return nil
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

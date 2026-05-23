package usagelog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Sink consumes usage events. Implementations are expected to be
// non-blocking from the Emitter drain goroutine's perspective — slow
// I/O belongs inside the sink's own buffering, not in the caller.
type Sink interface {
	// Write delivers one event. The error is logged by the Emitter;
	// returning non-nil does not stop subsequent events.
	Write(ev Event) error
}

// FileSink appends one JSONL line per event to an *os.File. Safe for
// concurrent Write; uses an internal mutex around the encoder.
//
// Buffering is the OS's job — we don't add a userland buffer because
// power-loss durability matters more than write throughput for usage
// events. Callers that want batched writes can wrap with a buffered
// writer behind the sink.
type FileSink struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
}

// NewFileSink opens (creating, append-mode) the given path for the
// FileSink. Caller is responsible for fsync semantics if needed.
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("usagelog.NewFileSink: open %q: %w", path, err)
	}
	return NewFileSinkFromWriter(f), nil
}

// NewFileSinkFromWriter wraps an existing writer (useful for tests
// that hand in *bytes.Buffer or io.Discard).
func NewFileSinkFromWriter(w io.Writer) *FileSink {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &FileSink{w: w, enc: enc}
}

// Write encodes ev as one JSON line. The json.Encoder appends '\n'
// after each value, so output is JSONL-compatible.
func (s *FileSink) Write(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(ev); err != nil {
		return fmt.Errorf("usagelog.FileSink.Write: %w", err)
	}
	return nil
}

// StdoutSink is a convenience FileSink wired to os.Stdout. Useful for
// development and CI where structured logs go to stdout for the
// container runtime to capture.
func StdoutSink() *FileSink { return NewFileSinkFromWriter(os.Stdout) }

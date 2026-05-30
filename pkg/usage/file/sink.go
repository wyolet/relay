// Package file is the JSONL file backend for usage logging: a Sink that
// appends one event per line and a Reader that scans the same file. It is
// the zero-dependency default backend — durable enough for single-pod and
// dogfood deployments, and the on-disk WAL the ClickHouse backend replays
// from. Implements usage.Sink and usage.Reader.
package file

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/wyolet/relay/pkg/usage"
)

var _ usage.Sink = (*Sink)(nil)

// Sink appends one JSONL line per event to an io.Writer. Safe for
// concurrent Write; uses an internal mutex around the encoder.
//
// Buffering is the OS's job — we don't add a userland buffer because
// power-loss durability matters more than write throughput for usage
// events. Callers that want batched writes wrap with a buffered writer.
type Sink struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
}

// NewSink opens (creating, append-mode) the given path for the Sink.
// Caller is responsible for fsync semantics if needed.
func NewSink(path string) (*Sink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("usage/file.NewSink: open %q: %w", path, err)
	}
	return NewSinkFromWriter(f), nil
}

// NewSinkFromWriter wraps an existing writer (useful for tests that hand
// in *bytes.Buffer or io.Discard).
func NewSinkFromWriter(w io.Writer) *Sink {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &Sink{w: w, enc: enc}
}

// Write encodes ev as one JSON line. The json.Encoder appends '\n' after
// each value, so output is JSONL-compatible.
func (s *Sink) Write(ev usage.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(ev); err != nil {
		return fmt.Errorf("usage/file.Sink.Write: %w", err)
	}
	return nil
}

// Stdout is a convenience Sink wired to os.Stdout. Useful for development
// and CI where structured logs go to stdout for the container runtime to
// capture.
func Stdout() *Sink { return NewSinkFromWriter(os.Stdout) }

// Package file is the JSONL file backend for usage logging: a Sink that
// appends one event per line and a Reader that scans the same file. It is
// the zero-dependency default backend — durable enough for single-pod and
// dogfood deployments, and the on-disk WAL the ClickHouse backend replays
// from. Implements usage.Sink and usage.Reader.
package file

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/wyolet/relay/pkg/usage"
)

var _ usage.Sink = (*Sink)(nil)
var _ usage.BatchWriter = (*Sink)(nil)

// batchWriteBytes bounds how much of a batch is encoded before it is handed
// to the writer.
const batchWriteBytes = 64 << 10

// Sink appends one JSONL line per event to an io.Writer. Safe for
// concurrent Write; uses an internal mutex around the encoder.
//
// Nothing is buffered across calls: every Write and WriteBatch has reached
// the writer when it returns, so a process crash loses nothing accepted.
type Sink struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder

	batch    bytes.Buffer
	batchEnc *json.Encoder
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
	s := &Sink{w: w, enc: enc}
	s.batchEnc = json.NewEncoder(&s.batch)
	s.batchEnc.SetEscapeHTML(false)
	return s
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

// WriteBatch encodes evs as JSON lines and hands them to the writer in
// whole-line chunks of about batchWriteBytes. An event that fails to encode
// is skipped and reported in the returned error.
func (s *Sink) WriteBatch(evs []usage.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var encErr error
	skipped := 0
	for i := range evs {
		if err := s.batchEnc.Encode(evs[i]); err != nil {
			skipped++
			if encErr == nil {
				encErr = err
			}
			continue
		}
		if s.batch.Len() >= batchWriteBytes {
			if err := s.writeBatchLocked(); err != nil {
				return err
			}
		}
	}
	if err := s.writeBatchLocked(); err != nil {
		return err
	}
	if encErr != nil {
		return fmt.Errorf("usage/file.Sink.WriteBatch: skipped %d of %d events: %w", skipped, len(evs), encErr)
	}
	return nil
}

func (s *Sink) writeBatchLocked() error {
	if s.batch.Len() == 0 {
		return nil
	}
	_, err := s.w.Write(s.batch.Bytes())
	s.batch.Reset()
	if err != nil {
		return fmt.Errorf("usage/file.Sink.WriteBatch: %w", err)
	}
	return nil
}

// Stdout is a convenience Sink wired to os.Stdout. Useful for development
// and CI where structured logs go to stdout for the container runtime to
// capture.
func Stdout() *Sink { return NewSinkFromWriter(os.Stdout) }

// Package file is the JSONL file backend for payload logging: one Record
// per line. The zero-dependency default backend; the s3 backend (which
// pulls a cloud SDK and is excluded from minimal builds) lives alongside
// in pkg/payload/s3. Implements payload.Sink.
package file

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/wyolet/relay/pkg/payload"
)

var _ payload.Sink = (*Sink)(nil)

// Sink appends one JSONL line per Record. Bodies serialize as base64
// (json's []byte encoding), so the file is lossless for any content.
// Safe for concurrent Write via an internal mutex. Buffering is the OS's
// job — no userland buffer.
type Sink struct {
	mu     sync.Mutex
	enc    *json.Encoder
	closer io.Closer
}

// NewSink opens (creating, append-mode) path for the sink.
func NewSink(path string) (*Sink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("payload/file.NewSink: open %q: %w", path, err)
	}
	s := NewSinkFromWriter(f)
	s.closer = f
	return s, nil
}

// NewSinkFromWriter wraps an existing writer (tests, stdout).
func NewSinkFromWriter(w io.Writer) *Sink {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &Sink{enc: enc}
}

func (s *Sink) Write(r payload.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(r); err != nil {
		return fmt.Errorf("payload/file.Sink.Write: %w", err)
	}
	return nil
}

// Close closes the underlying file (if NewSink opened one). Satisfies
// payload.Closer so the emitter flushes on shutdown.
func (s *Sink) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

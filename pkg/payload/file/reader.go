package file

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/wyolet/relay/pkg/payload"
)

var _ payload.Reader = (*Reader)(nil)

// Reader is a payload.Reader backed by the same JSONL file Sink writes to.
// Get scans the file for the request id — fine for dogfood files (MB range).
// There is no List: the Logs list is served by the log store, not the body
// store.
type Reader struct {
	path string
}

// NewReader constructs a Reader for path. Path must match the Sink the relay
// is currently writing to.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// Get scans for the record with the given request id, returning the newest
// match (a re-run reusing an id). Returns payload.ErrNotFound when absent or
// when the file doesn't exist yet.
func (r *Reader) Get(_ context.Context, requestID string) (payload.Record, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return payload.Record{}, payload.ErrNotFound
		}
		return payload.Record{}, fmt.Errorf("payload/file.Reader: open %q: %w", r.path, err)
	}
	defer f.Close()

	var found *payload.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // bodies can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec payload.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // skip malformed lines rather than failing the lookup
		}
		if rec.RequestID != requestID {
			continue
		}
		if found == nil || rec.Timestamp.After(found.Timestamp) {
			r := rec
			found = &r
		}
	}
	if err := sc.Err(); err != nil {
		return payload.Record{}, fmt.Errorf("payload/file.Reader: scan: %w", err)
	}
	if found == nil {
		return payload.Record{}, payload.ErrNotFound
	}
	return *found, nil
}

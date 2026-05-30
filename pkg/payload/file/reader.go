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
// Linear scan per query — fine for dogfood files (MB range, thousands of
// records). For production-scale capture, use the object-store backend.
type Reader struct {
	path string
}

// NewReader constructs a Reader for path. Path must match the Sink the
// relay is currently writing to.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// List scans the file, applies filters + keyset cursor, and returns the
// newest matching records up to q.Limit with bodies stripped.
func (r *Reader) List(_ context.Context, q payload.Query) ([]payload.Record, error) {
	all, err := r.scan()
	if err != nil {
		return nil, err
	}
	matched := payload.SortAndLimit(payload.FilterRecords(all, q), q.Limit)
	for i := range matched {
		matched[i] = payload.StripBody(matched[i])
	}
	return matched, nil
}

// Get scans for the record with the given request id and returns it with
// bodies intact. The newest match wins if the file somehow holds duplicates
// (a re-run reusing an id). Returns payload.ErrNotFound when absent.
func (r *Reader) Get(_ context.Context, requestID string) (payload.Record, error) {
	all, err := r.scan()
	if err != nil {
		return payload.Record{}, err
	}
	var found *payload.Record
	for i := range all {
		if all[i].RequestID != requestID {
			continue
		}
		if found == nil || all[i].Timestamp.After(found.Timestamp) {
			rec := all[i]
			found = &rec
		}
	}
	if found == nil {
		return payload.Record{}, payload.ErrNotFound
	}
	return *found, nil
}

// scan reads every record in the file. A missing file yields an empty
// slice, not an error — useful at boot before any capture has fired.
func (r *Reader) scan() ([]payload.Record, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("payload/file.Reader: open %q: %w", r.path, err)
	}
	defer f.Close()

	var out []payload.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // bodies can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec payload.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// Skip malformed lines rather than failing the whole query.
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("payload/file.Reader: scan: %w", err)
	}
	return out, nil
}

// Package clickhouse provides a ClickHouse-backed implementation of
// payload.Sink, payload.Reader, and payload.Closer — the "text bodies in
// ClickHouse" backend for the Logs view. Metadata + bodies live in one
// MergeTree table (bodies as ZSTD String columns); List projects only the
// metadata columns, Get fetches the body columns by request_id.
//
// Durability comes from a WAL-segment queue (pkg/wal): records append to an
// active segment; full segments rotate and flush to ClickHouse; a segment is
// deleted only after CH confirms the insert; leftover segments are drained on
// boot. Rotation also triggers on a byte threshold — payload bodies are
// MB-scale, so a line-count cap alone would let a segment grow to gigabytes.
//
// Out of scope: schema migrations (IF NOT EXISTS), dedup of replayed segments
// (idempotency is the caller's concern), multi-node coordination (each pod
// runs its own WAL in its own directory).
package clickhouse

import (
	"log/slog"
	"time"

	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/wal"
)

// segmentQueue is the sink's write-ahead log: the shared queue carrying
// payload records.
type segmentQueue = wal.Queue[payload.Record]

func newSegmentQueue(
	dir string,
	maxLines, maxBytes int,
	flushInterval time.Duration,
	maxSegments int,
	log *slog.Logger,
	flushFn func([]payload.Record) error,
) (*segmentQueue, error) {
	return wal.OpenRetaining(wal.Config{
		Dir:           dir,
		MaxLines:      maxLines,
		MaxBytes:      maxBytes,
		FlushInterval: flushInterval,
		MaxSegments:   maxSegments,
		// Bodies are MB-scale: decoding and inserting a whole tick's worth at
		// once spikes heap and CPU on a busy pod.
		FlushOnRotate: true,
		Kind:          "payload",
		Logger:        log,
	}, flushFn, decodedRecord)
}

// decodedRecord returns r as json.Unmarshal of its WAL line yields it, so the
// queue can flush r from memory. omitempty drops empty bodies, which decode
// as nil; every other field decodes to the value written.
func decodedRecord(r payload.Record) (payload.Record, bool) {
	ts, ok := wal.DecodedTime(r.Timestamp)
	if !ok {
		return payload.Record{}, false
	}
	r.Timestamp = ts
	if len(r.RequestBody) == 0 {
		r.RequestBody = nil
	}
	if len(r.ResponseBody) == 0 {
		r.ResponseBody = nil
	}
	return r, true
}

// Package clickhouse provides a ClickHouse-backed implementation of
// usage.Sink, usage.Reader, and usage.Closer.
//
// Durability is provided by a WAL-segment queue (pkg/wal): events are
// appended to an active segment file; full segments are rotated and flushed
// to ClickHouse; a segment is deleted only after ClickHouse confirms the
// insert. Crash recovery is automatic — leftover segments from a previous run
// are drained on startup.
//
// Out of scope: schema migrations (the table is created with IF NOT EXISTS),
// deduplication of replayed segments (idempotency is the caller's concern),
// and multi-node coordination (each relay pod runs its own WAL in its own
// directory).
package clickhouse

import (
	"log/slog"
	"time"

	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/wal"
)

// segmentQueue is the sink's write-ahead log: the shared queue carrying
// usage events. No byte cap — events are small, the line cap bounds a
// segment on its own.
type segmentQueue = wal.Queue[usage.Event]

// flushWorkers is how many segments the WAL decodes and inserts at once. One
// at a time, the drain decodes on a single core and idles through every
// insert round trip, which caps it below what a busy pod writes.
const flushWorkers = 4

func newSegmentQueue(
	dir string,
	maxLines int,
	flushInterval time.Duration,
	maxSegments int,
	log *slog.Logger,
	flushFn func([]usage.Event) error,
) (*segmentQueue, error) {
	return wal.Open(wal.Config{
		Dir:           dir,
		MaxLines:      maxLines,
		FlushInterval: flushInterval,
		MaxSegments:   maxSegments,
		FlushWorkers:  flushWorkers,
		Kind:          "usage",
		Logger:        log,
	}, flushFn)
}

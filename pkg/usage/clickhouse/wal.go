// Package clickhouse provides a ClickHouse-backed implementation of
// usage.Sink, usage.Reader, and usage.Closer.
//
// Durability is provided by a WAL-segment queue: events are appended to an
// active segment file; full segments are rotated and flushed to ClickHouse;
// a segment is deleted only after ClickHouse confirms the insert. Crash
// recovery is automatic — leftover segments from a previous run are drained
// on startup.
//
// Out of scope: schema migrations (the table is created with IF NOT EXISTS),
// deduplication of replayed segments (idempotency is the caller's concern),
// and multi-node coordination (each relay pod runs its own WAL in its own
// directory).
package clickhouse

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

const (
	activeName    = "active.jsonl"
	segmentGlob   = "segment-*.jsonl"
	segmentPrefix = "segment-"
)

// segmentQueue is the WAL layer. It knows nothing about ClickHouse — the
// flushFn injects the remote-write operation so the queue is testable
// without a running server.
type segmentQueue struct {
	dir         string
	maxLines    int
	maxSegments int
	flushFn     func([]usage.Event) error
	log         *slog.Logger

	mu     sync.Mutex
	active *os.File
	writer *bufio.Writer
	lines  int

	// flushMu serializes flushPending so the boot-time Recover() and the
	// background ticker can't process (and double-insert) the same segment.
	flushMu sync.Mutex

	dropped atomic.Uint64

	ticker *time.Ticker
	stop   chan struct{}
	done   chan struct{}
}

func newSegmentQueue(
	dir string,
	maxLines int,
	flushInterval time.Duration,
	maxSegments int,
	log *slog.Logger,
	flushFn func([]usage.Event) error,
) (*segmentQueue, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("usage/clickhouse: wal mkdir: %w", err)
	}
	q := &segmentQueue{
		dir:         dir,
		maxLines:    maxLines,
		maxSegments: maxSegments,
		flushFn:     flushFn,
		log:         log,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	if err := q.openActive(); err != nil {
		return nil, err
	}
	q.ticker = time.NewTicker(flushInterval)
	go q.background()
	return q, nil
}

// openActive opens (or creates) the active segment file, appending to any
// existing content from a previous run. The caller holds no lock — this is
// called only from the constructor and from within rotate (which holds mu).
func (q *segmentQueue) openActive() error {
	path := filepath.Join(q.dir, activeName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("usage/clickhouse: open active segment: %w", err)
	}

	// Count existing lines so the rotation threshold stays accurate.
	n, err := countLines(path)
	if err != nil {
		f.Close()
		return fmt.Errorf("usage/clickhouse: count active lines: %w", err)
	}

	q.active = f
	q.writer = bufio.NewWriter(f)
	q.lines = n
	return nil
}

// Write appends ev to the active segment. Rotation happens synchronously
// when maxLines is reached.
func (q *segmentQueue) Write(ev usage.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("usage/clickhouse: marshal event: %w", err)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if _, err := q.writer.Write(b); err != nil {
		return err
	}
	if err := q.writer.WriteByte('\n'); err != nil {
		return err
	}
	q.lines++

	// Flush to the OS on every write so a process crash / SIGKILL loses
	// nothing already accepted. (No fsync — power-off may lose the last
	// OS-buffered window; that's the accepted best-effort bound, recovered
	// only if the bytes reached disk.)
	if err := q.writer.Flush(); err != nil {
		return err
	}

	if q.lines >= q.maxLines {
		if err := q.rotateLocked(); err != nil {
			q.log.Warn("usage/clickhouse: rotate on full", "err", err)
		}
	}
	return nil
}

// rotateLocked closes the active file, renames it to a timestamped segment,
// and opens a fresh active file. Must be called with mu held.
func (q *segmentQueue) rotateLocked() error {
	if q.lines == 0 {
		return nil
	}

	if err := q.writer.Flush(); err != nil {
		return err
	}
	if err := q.active.Close(); err != nil {
		return err
	}

	src := filepath.Join(q.dir, activeName)
	dst := filepath.Join(q.dir, fmt.Sprintf("%s%d.jsonl", segmentPrefix, time.Now().UnixNano()))
	if err := os.Rename(src, dst); err != nil {
		return err
	}

	return q.openActive()
}

// Recover drains any segments left from a previous run (including an
// active.jsonl that was never cleanly rotated). Call once after
// newSegmentQueue — the constructor does not call it automatically so
// callers can inject an error handler between construction and recovery.
func (q *segmentQueue) Recover() {
	// Rename any leftover active.jsonl into a segment first so it joins
	// the normal flush queue.
	q.mu.Lock()
	err := q.rotateLocked()
	q.mu.Unlock()
	if err != nil {
		q.log.Warn("usage/clickhouse: recover: rotate active", "err", err)
	}
	q.flushPending()
}

func (q *segmentQueue) background() {
	defer close(q.done)
	for {
		select {
		case <-q.stop:
			return
		case <-q.ticker.C:
			q.mu.Lock()
			err := q.rotateLocked()
			q.mu.Unlock()
			if err != nil {
				q.log.Warn("usage/clickhouse: tick rotate", "err", err)
			}
			q.flushPending()
		}
	}
}

// flushPending finds all segment-*.jsonl files and flushes them in order.
// Before flushing it enforces maxSegments by dropping the oldest excess.
func (q *segmentQueue) flushPending() {
	q.flushMu.Lock()
	defer q.flushMu.Unlock()

	segments, err := q.listSegments()
	if err != nil {
		q.log.Warn("usage/clickhouse: list segments", "err", err)
		return
	}

	// Enforce disk cap — drop oldest excess segments.
	if q.maxSegments > 0 && len(segments) > q.maxSegments {
		excess := segments[:len(segments)-q.maxSegments]
		for _, s := range excess {
			if rerr := os.Remove(s); rerr == nil {
				q.log.Warn("usage/clickhouse: dropped oldest segment (maxSegments exceeded)", "file", filepath.Base(s))
				q.dropped.Add(1)
			}
		}
		segments = segments[len(segments)-q.maxSegments:]
	}

	for _, seg := range segments {
		events, err := readSegment(seg)
		if err != nil {
			q.log.Warn("usage/clickhouse: read segment", "file", filepath.Base(seg), "err", err)
			return
		}
		if err := q.flushFn(events); err != nil {
			// Do NOT delete — leave for retry on next tick.
			q.log.Warn("usage/clickhouse: flush failed, will retry", "file", filepath.Base(seg), "err", err)
			return
		}
		if err := os.Remove(seg); err != nil {
			q.log.Warn("usage/clickhouse: remove segment", "file", filepath.Base(seg), "err", err)
		}
	}
}

// listSegments returns segment paths sorted oldest-first (by the embedded
// unix-nano timestamp in the filename).
func (q *segmentQueue) listSegments() ([]string, error) {
	entries, err := fs.Glob(os.DirFS(q.dir), segmentGlob)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, filepath.Join(q.dir, e))
	}
	slices.SortFunc(paths, func(a, b string) int {
		return segmentNano(a) - segmentNano(b)
	})
	return paths, nil
}

// segmentNano extracts the unix-nano suffix from a segment filename for
// stable chronological sort. Returns 0 on parse failure.
func segmentNano(path string) int {
	base := filepath.Base(path)
	s := strings.TrimPrefix(base, segmentPrefix)
	s = strings.TrimSuffix(s, ".jsonl")
	n, _ := strconv.Atoi(s)
	return n
}

// Close stops the background goroutine, flushes the active segment, and
// runs a final flushPending. Unacknowledged segments remain on disk for
// the next boot to recover.
func (q *segmentQueue) Close() error {
	q.ticker.Stop()
	close(q.stop)
	<-q.done

	q.mu.Lock()
	err := q.rotateLocked()
	q.mu.Unlock()
	if err != nil {
		q.log.Warn("usage/clickhouse: close rotate", "err", err)
	}

	q.flushPending()
	return nil
}

// Dropped returns the number of events dropped due to maxSegments enforcement.
func (q *segmentQueue) Dropped() uint64 {
	return q.dropped.Load()
}

func readSegment(path string) ([]usage.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []usage.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev usage.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("decode line: %w", err)
		}
		events = append(events, ev)
	}
	return events, sc.Err()
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n, sc.Err()
}

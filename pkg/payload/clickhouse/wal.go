// Package clickhouse provides a ClickHouse-backed implementation of
// payload.Sink, payload.Reader, and payload.Closer — the Langfuse-style
// "text bodies in ClickHouse" backend for the Logs view. Metadata + bodies
// live in one MergeTree table (bodies as ZSTD String columns); List projects
// only the metadata columns, Get fetches the body columns by request_id.
//
// Durability mirrors the usage CH sink: a WAL-segment queue. Records append
// to an active segment; full segments rotate and flush to ClickHouse; a
// segment is deleted only after CH confirms the insert; leftover segments are
// drained on boot. Unlike the usage WAL, rotation also triggers on a byte
// threshold — payload bodies are MB-scale, so a line-count cap alone would
// let a segment grow to gigabytes.
//
// Out of scope: schema migrations (IF NOT EXISTS), dedup of replayed segments
// (idempotency is the caller's concern), multi-node coordination (each pod
// runs its own WAL in its own directory).
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

	"github.com/wyolet/relay/pkg/payload"
)

const (
	activeName    = "active.jsonl"
	segmentGlob   = "segment-*.jsonl"
	segmentPrefix = "segment-"

	// scanBuf bounds a single WAL line. A record's bodies are base64 in JSON
	// and the provider request ceiling is ~32 MB, so allow generous headroom.
	scanBuf = 48 << 20
)

// segmentQueue is the WAL layer. It knows nothing about ClickHouse — the
// flushFn injects the remote-write so the queue is testable without a server.
type segmentQueue struct {
	dir         string
	maxLines    int
	maxBytes    int
	maxSegments int
	flushFn     func([]payload.Record) error
	log         *slog.Logger

	mu     sync.Mutex
	active *os.File
	writer *bufio.Writer
	lines  int
	bytes  int

	// flushMu serializes flushPending so boot-time Recover() and the
	// background ticker can't process (and double-insert) the same segment.
	flushMu sync.Mutex

	dropped atomic.Uint64

	ticker *time.Ticker
	stop   chan struct{}
	done   chan struct{}
}

func newSegmentQueue(
	dir string,
	maxLines, maxBytes int,
	flushInterval time.Duration,
	maxSegments int,
	log *slog.Logger,
	flushFn func([]payload.Record) error,
) (*segmentQueue, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("payload/clickhouse: wal mkdir: %w", err)
	}
	q := &segmentQueue{
		dir:         dir,
		maxLines:    maxLines,
		maxBytes:    maxBytes,
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

// openActive opens (or creates) the active segment, appending to any existing
// content from a previous run. Called only from the constructor and rotate
// (which holds mu).
func (q *segmentQueue) openActive() error {
	path := filepath.Join(q.dir, activeName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("payload/clickhouse: open active segment: %w", err)
	}

	n, err := countLines(path)
	if err != nil {
		f.Close()
		return fmt.Errorf("payload/clickhouse: count active lines: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("payload/clickhouse: stat active segment: %w", err)
	}

	q.active = f
	q.writer = bufio.NewWriter(f)
	q.lines = n
	q.bytes = int(info.Size())
	return nil
}

// Write appends r to the active segment. Rotation happens synchronously when
// maxLines OR maxBytes is reached.
func (q *segmentQueue) Write(r payload.Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("payload/clickhouse: marshal record: %w", err)
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
	q.bytes += len(b) + 1

	// Flush to the OS on every write so a crash / SIGKILL loses nothing
	// already accepted. (No fsync — power-off may lose the last OS-buffered
	// window; accepted best-effort bound.)
	if err := q.writer.Flush(); err != nil {
		return err
	}

	if q.lines >= q.maxLines || q.bytes >= q.maxBytes {
		if err := q.rotateLocked(); err != nil {
			q.log.Warn("payload/clickhouse: rotate on full", "err", err)
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
// active.jsonl never cleanly rotated). Call once after newSegmentQueue.
func (q *segmentQueue) Recover() {
	q.mu.Lock()
	err := q.rotateLocked()
	q.mu.Unlock()
	if err != nil {
		q.log.Warn("payload/clickhouse: recover: rotate active", "err", err)
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
				q.log.Warn("payload/clickhouse: tick rotate", "err", err)
			}
			q.flushPending()
		}
	}
}

// flushPending finds all segment files and flushes them oldest-first. Before
// flushing it enforces maxSegments by dropping the oldest excess.
func (q *segmentQueue) flushPending() {
	q.flushMu.Lock()
	defer q.flushMu.Unlock()

	segments, err := q.listSegments()
	if err != nil {
		q.log.Warn("payload/clickhouse: list segments", "err", err)
		return
	}

	if q.maxSegments > 0 && len(segments) > q.maxSegments {
		excess := segments[:len(segments)-q.maxSegments]
		for _, s := range excess {
			if rerr := os.Remove(s); rerr == nil {
				q.log.Warn("payload/clickhouse: dropped oldest segment (maxSegments exceeded)", "file", filepath.Base(s))
				q.dropped.Add(1)
			}
		}
		segments = segments[len(segments)-q.maxSegments:]
	}

	for _, seg := range segments {
		records, err := readSegment(seg)
		if err != nil {
			q.log.Warn("payload/clickhouse: read segment", "file", filepath.Base(seg), "err", err)
			return
		}
		if err := q.flushFn(records); err != nil {
			q.log.Warn("payload/clickhouse: flush failed, will retry", "file", filepath.Base(seg), "err", err)
			return
		}
		if err := os.Remove(seg); err != nil {
			q.log.Warn("payload/clickhouse: remove segment", "file", filepath.Base(seg), "err", err)
		}
	}
}

// listSegments returns segment paths sorted oldest-first by the embedded
// unix-nano timestamp.
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

// segmentNano extracts the unix-nano suffix for a stable chronological sort.
func segmentNano(path string) int {
	base := filepath.Base(path)
	s := strings.TrimPrefix(base, segmentPrefix)
	s = strings.TrimSuffix(s, ".jsonl")
	n, _ := strconv.Atoi(s)
	return n
}

// Close stops the background goroutine, rotates the active segment, and runs a
// final flushPending. Unacknowledged segments remain on disk for the next boot.
func (q *segmentQueue) Close() error {
	q.ticker.Stop()
	close(q.stop)
	<-q.done

	q.mu.Lock()
	err := q.rotateLocked()
	q.mu.Unlock()
	if err != nil {
		q.log.Warn("payload/clickhouse: close rotate", "err", err)
	}
	q.flushPending()
	return nil
}

// Dropped returns the number of records dropped due to maxSegments enforcement.
func (q *segmentQueue) Dropped() uint64 { return q.dropped.Load() }

func readSegment(path string) ([]payload.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []payload.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuf)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r payload.Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("decode line: %w", err)
		}
		records = append(records, r)
	}
	return records, sc.Err()
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
	sc.Buffer(make([]byte, 0, 64*1024), scanBuf)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n, sc.Err()
}

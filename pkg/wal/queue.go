// Package wal is a durable write-ahead log for asynchronous sinks. Records
// are JSON-appended to an active segment file; a segment that hits the line
// (or byte) cap is rotated and handed to the sink's flush function, and is
// deleted only once that flush succeeds. Segments left by a previous run are
// replayed by Recover, so a crash costs only what never reached the disk.
//
// The queue knows nothing about the sink: the record type is a type
// parameter and the remote write is the injected flush function, so it is
// testable without a server.
//
// Out of scope: deduplication of replayed segments (idempotency is the
// sink's concern), fsync-level durability (a power cut may lose the last
// OS-buffered window), and multi-node coordination — each process owns its
// own directory.
package wal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// writeBufferSize bounds how much of a batch reaches the OS per write call.
const writeBufferSize = 64 << 10

// Config holds the queue's tunables. Dir, FlushInterval, MaxLines and Logger
// are required; MaxBytes and MaxSegments may be left at zero to disable the
// byte cap and the disk cap respectively.
type Config struct {
	// Dir is the directory holding the active segment and the pending ones.
	Dir string

	// MaxLines rotates the active segment once it holds this many records.
	MaxLines int

	// MaxBytes rotates the active segment once it reaches this size,
	// whichever comes first with MaxLines. 0 disables the byte cap.
	MaxBytes int

	// FlushInterval is how often the background goroutine rotates and
	// flushes pending segments.
	FlushInterval time.Duration

	// MaxSegments caps how many pending segments may accumulate on disk.
	// When exceeded the oldest are dropped, counted in Dropped(), and their
	// records in relay_records_lost_total{kind=Kind}. 0 disables the cap.
	MaxSegments int

	// FlushOnRotate hands a segment to the flush function as soon as it
	// rotates on a full cap instead of on the next tick, so a busy queue
	// flushes in segment-sized steps rather than one tick-sized burst. After a
	// failed flush only the tick retries.
	FlushOnRotate bool

	// Kind labels the flush-lag metric and this queue's log lines
	// ("usage", "payload").
	Kind string

	Logger *slog.Logger
}

// Queue is the WAL for records of type T.
type Queue[T any] struct {
	cfg   Config
	flush func([]T) error
	log   *slog.Logger

	mu     sync.Mutex
	active *os.File
	writer *bufio.Writer
	lines  int
	bytes  int

	// line holds the record being appended, encoded by encoder; reused
	// under mu and copied into writer before the append returns.
	line    bytes.Buffer
	encoder *json.Encoder

	lastSegmentNano int64
	// segments holds what this process knows of each segment it rotated,
	// keyed by path: the line count, so the read-back sizes its slice once,
	// and the records held for its first flush.
	segments map[string]rotatedSegment[T]

	// decoded, set by OpenRetaining, gives a record as its line decodes.
	// While retaining, retained mirrors every line of the active segment;
	// retainedBytes counts it and every held rotated segment against
	// retainLimit (maxRetainedBytes).
	decoded             func(T) (T, bool)
	recordSize          int
	retainLimit         int
	retaining           bool
	retained            []T
	activeRetainedBytes int
	retainedBytes       int
	// retainCap sizes the next held slice from the last rotated segment.
	retainCap int

	// flushMu serializes FlushPending so the boot-time Recover() and the
	// background ticker can't process (and double-insert) the same segment.
	flushMu sync.Mutex

	dropped atomic.Uint64
	// failing is set while the last flush pass failed, so rotations stop
	// triggering retries.
	failing atomic.Bool
	rotated chan struct{}

	ticker *time.Ticker
	stop   chan struct{}
	done   chan struct{}
}

// Open creates the directory, opens the active segment, and starts the
// background rotate/flush ticker. Call Recover once afterwards to drain
// segments left by a previous run — the constructor does not, so callers can
// finish wiring the sink in between.
func Open[T any](cfg Config, flush func([]T) error) (*Queue[T], error) {
	return OpenRetaining(cfg, flush, nil)
}

// OpenRetaining is Open for a queue that hands a segment it rotated to flush
// from memory instead of reading the file back. decoded returns a record as
// json.Unmarshal of its line yields it (see DecodedTime), or false when only
// decoding can tell; that segment then flushes from disk. The file stays the
// source of truth: it is deleted only after a successful flush, and Recover
// and every retry after a failed flush read it. Write and WriteBatch keep
// referencing the records' maps, slices and pointers until the flush, so the
// caller must not modify them afterwards.
func OpenRetaining[T any](cfg Config, flush func([]T) error, decoded func(T) (T, bool)) (*Queue[T], error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal/%s: mkdir: %w", cfg.Kind, err)
	}
	q := &Queue[T]{
		cfg:         cfg,
		flush:       flush,
		log:         cfg.Logger.With("wal", cfg.Kind),
		segments:    make(map[string]rotatedSegment[T]),
		decoded:     decoded,
		recordSize:  int(reflect.TypeFor[T]().Size()),
		retainLimit: maxRetainedBytes,
		retainCap:   cfg.MaxLines,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		rotated:     make(chan struct{}, 1),
	}
	q.encoder = json.NewEncoder(&q.line)
	if err := q.openActive(); err != nil {
		return nil, err
	}
	q.ticker = time.NewTicker(cfg.FlushInterval)
	go q.background()
	return q, nil
}

// openActive opens (or creates) the active segment file, appending to any
// existing content from a previous run. Called only from Open and from
// rotateLocked (which holds mu).
func (q *Queue[T]) openActive() error {
	path := filepath.Join(q.cfg.Dir, ActiveName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("wal/%s: open active segment: %w", q.cfg.Kind, err)
	}

	// Count existing lines so the rotation threshold stays accurate.
	n, err := CountLines(path)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("wal/%s: count active lines: %w", q.cfg.Kind, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("wal/%s: stat active segment: %w", q.cfg.Kind, err)
	}

	q.active = f
	if q.writer == nil {
		q.writer = bufio.NewWriterSize(f, writeBufferSize)
	} else {
		q.writer.Reset(f)
	}
	q.lines = n
	q.bytes = int(info.Size())
	q.startRetainingLocked(info.Size())
	return nil
}

// Write appends rec to the active segment. Rotation happens synchronously
// when MaxLines (or MaxBytes) is reached.
func (q *Queue[T]) Write(rec T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.releaseLineLocked()

	b, err := q.encodeLocked(rec)
	if err != nil {
		return fmt.Errorf("wal/%s: marshal record: %w", q.cfg.Kind, err)
	}
	if err := q.appendLocked(rec, b); err != nil {
		return err
	}
	return q.flushWriterLocked()
}

// WriteBatch appends recs in order under one lock and flushes once, rotating
// mid-batch wherever Write would have. A record that fails to marshal is
// skipped and reported in the returned error; the rest are still written.
func (q *Queue[T]) WriteBatch(recs []T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.releaseLineLocked()

	var marshalErr error
	skipped := 0
	for i := range recs {
		b, err := q.encodeLocked(recs[i])
		if err != nil {
			skipped++
			if marshalErr == nil {
				marshalErr = err
			}
			continue
		}
		if err := q.appendLocked(recs[i], b); err != nil {
			return err
		}
	}
	if err := q.flushWriterLocked(); err != nil {
		return err
	}
	if marshalErr != nil {
		return fmt.Errorf("wal/%s: marshal: skipped %d of %d records: %w", q.cfg.Kind, skipped, len(recs), marshalErr)
	}
	return nil
}

// encodeLocked encodes rec into q.line as json.Marshal does, plus the
// trailing newline. The result is valid until the next call. Must be called
// with mu held.
func (q *Queue[T]) encodeLocked(rec T) ([]byte, error) {
	q.line.Reset()
	if err := q.encoder.Encode(rec); err != nil {
		return nil, err
	}
	return q.line.Bytes(), nil
}

// releaseLineLocked drops a line buffer grown past the write buffer, so one
// MB-scale payload record isn't kept alive between writes. Must be called
// with mu held.
func (q *Queue[T]) releaseLineLocked() {
	if q.line.Cap() > writeBufferSize {
		q.line = bytes.Buffer{}
	}
}

// flushWriterLocked flushes the writer. After a failed write the file's
// content is unknown, so the active segment stops being held in memory.
// Must be called with mu held.
func (q *Queue[T]) flushWriterLocked() error {
	if err := q.writer.Flush(); err != nil {
		q.stopRetainingLocked()
		return err
	}
	return nil
}

// appendLocked buffers rec's newline-terminated line and rotates once
// the segment is full. Records count as accepted only after the caller's
// Flush returns: no fsync, so a process crash loses nothing accepted, a power
// cut may lose the last OS-buffered window. Must be called with mu held.
func (q *Queue[T]) appendLocked(rec T, line []byte) error {
	// Hand the OS whole lines, so a crash mid-batch tears at most the record
	// being written instead of one that straddles two writes.
	if q.writer.Buffered() > 0 && q.writer.Available() < len(line) {
		if err := q.flushWriterLocked(); err != nil {
			return err
		}
	}
	// bufio copies line or writes it through before returning; it keeps no
	// reference, so q.line is free for the next record.
	if _, err := q.writer.Write(line); err != nil {
		q.stopRetainingLocked()
		return err
	}
	q.lines++
	q.bytes += len(line)
	q.retainLocked(rec, line)

	if q.lines >= q.cfg.MaxLines || (q.cfg.MaxBytes > 0 && q.bytes >= q.cfg.MaxBytes) {
		if err := q.rotateLocked(); err != nil {
			q.log.Warn("wal: rotate on full", "err", err)
		} else if q.cfg.FlushOnRotate && !q.failing.Load() {
			select {
			case q.rotated <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

// rotateLocked closes the active file, renames it to a timestamped segment,
// and opens a fresh active file. Must be called with mu held.
func (q *Queue[T]) rotateLocked() error {
	if q.lines == 0 {
		return nil
	}

	if err := q.flushWriterLocked(); err != nil {
		return err
	}
	if err := q.active.Close(); err != nil {
		q.stopRetainingLocked()
		return err
	}

	src := filepath.Join(q.cfg.Dir, ActiveName)
	// Rotations inside one batch can land on the same clock reading; a
	// repeated name would rename over the previous segment.
	nano := max(time.Now().UnixNano(), q.lastSegmentNano+1)
	q.lastSegmentNano = nano
	dst := filepath.Join(q.cfg.Dir, fmt.Sprintf("%s%d.jsonl", segmentPrefix, nano))
	if err := os.Rename(src, dst); err != nil {
		q.stopRetainingLocked()
		return err
	}
	seg := rotatedSegment[T]{lines: q.lines}
	if q.retaining {
		seg.records, seg.bytes = q.retained, q.activeRetainedBytes
		q.retained, q.activeRetainedBytes, q.retaining = nil, 0, false
	}
	q.segments[dst] = seg
	q.retainCap = q.lines

	return q.openActive()
}

// Recover drains any segments left from a previous run (including an
// active segment that was never cleanly rotated).
func (q *Queue[T]) Recover() {
	// Rename any leftover active segment first so it joins the normal
	// flush queue.
	q.mu.Lock()
	err := q.rotateLocked()
	q.mu.Unlock()
	if err != nil {
		q.log.Warn("wal: recover: rotate active", "err", err)
	}
	q.FlushPending()
}

func (q *Queue[T]) background() {
	defer close(q.done)
	for {
		select {
		case <-q.stop:
			return
		case <-q.rotated:
			q.FlushPending()
		case <-q.ticker.C:
			q.mu.Lock()
			err := q.rotateLocked()
			q.mu.Unlock()
			if err != nil {
				q.log.Warn("wal: tick rotate", "err", err)
			}
			q.FlushPending()
		}
	}
}

// StopBackground stops the rotate/flush ticker without a final flush — the
// crash-equivalent shutdown. Close is the normal path.
func (q *Queue[T]) StopBackground() {
	q.ticker.Stop()
	close(q.stop)
	<-q.done
}

// Close stops the background goroutine, rotates the active segment, and runs
// a final flush pass. Unacknowledged segments remain on disk for the next
// boot to recover.
func (q *Queue[T]) Close() error {
	q.StopBackground()

	q.mu.Lock()
	err := q.rotateLocked()
	q.mu.Unlock()
	if err != nil {
		q.log.Warn("wal: close rotate", "err", err)
	}

	q.FlushPending()
	return nil
}

// Dropped returns the number of segments dropped due to MaxSegments
// enforcement.
func (q *Queue[T]) Dropped() uint64 {
	return q.dropped.Load()
}

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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

	lastSegmentNano int64

	// flushMu serializes FlushPending so the boot-time Recover() and the
	// background ticker can't process (and double-insert) the same segment.
	flushMu sync.Mutex

	dropped atomic.Uint64

	ticker *time.Ticker
	stop   chan struct{}
	done   chan struct{}
}

// Open creates the directory, opens the active segment, and starts the
// background rotate/flush ticker. Call Recover once afterwards to drain
// segments left by a previous run — the constructor does not, so callers can
// finish wiring the sink in between.
func Open[T any](cfg Config, flush func([]T) error) (*Queue[T], error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal/%s: mkdir: %w", cfg.Kind, err)
	}
	q := &Queue[T]{
		cfg:   cfg,
		flush: flush,
		log:   cfg.Logger.With("wal", cfg.Kind),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
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
	return nil
}

// Write appends rec to the active segment. Rotation happens synchronously
// when MaxLines (or MaxBytes) is reached.
func (q *Queue[T]) Write(rec T) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("wal/%s: marshal record: %w", q.cfg.Kind, err)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if err := q.appendLocked(b); err != nil {
		return err
	}
	return q.writer.Flush()
}

// WriteBatch appends recs in order under one lock and flushes once, rotating
// mid-batch wherever Write would have. A record that fails to marshal is
// skipped and reported in the returned error; the rest are still written.
func (q *Queue[T]) WriteBatch(recs []T) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	var marshalErr error
	skipped := 0
	for i := range recs {
		b, err := json.Marshal(recs[i])
		if err != nil {
			skipped++
			if marshalErr == nil {
				marshalErr = err
			}
			continue
		}
		if err := q.appendLocked(b); err != nil {
			return err
		}
	}
	if err := q.writer.Flush(); err != nil {
		return err
	}
	if marshalErr != nil {
		return fmt.Errorf("wal/%s: marshal: skipped %d of %d records: %w", q.cfg.Kind, skipped, len(recs), marshalErr)
	}
	return nil
}

// appendLocked buffers one record line and rotates once the segment is full.
// Records count as accepted only after the caller's Flush returns: no fsync,
// so a process crash loses nothing accepted, a power cut may lose the last
// OS-buffered window. Must be called with mu held.
func (q *Queue[T]) appendLocked(b []byte) error {
	// Hand the OS whole lines, so a crash mid-batch tears at most the record
	// being written instead of one that straddles two writes.
	if q.writer.Buffered() > 0 && q.writer.Available() < len(b)+1 {
		if err := q.writer.Flush(); err != nil {
			return err
		}
	}
	if _, err := q.writer.Write(b); err != nil {
		return err
	}
	if err := q.writer.WriteByte('\n'); err != nil {
		return err
	}
	q.lines++
	q.bytes += len(b) + 1

	if q.lines >= q.cfg.MaxLines || (q.cfg.MaxBytes > 0 && q.bytes >= q.cfg.MaxBytes) {
		if err := q.rotateLocked(); err != nil {
			q.log.Warn("wal: rotate on full", "err", err)
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

	if err := q.writer.Flush(); err != nil {
		return err
	}
	if err := q.active.Close(); err != nil {
		return err
	}

	src := filepath.Join(q.cfg.Dir, ActiveName)
	// Rotations inside one batch can land on the same clock reading; a
	// repeated name would rename over the previous segment.
	nano := max(time.Now().UnixNano(), q.lastSegmentNano+1)
	q.lastSegmentNano = nano
	dst := filepath.Join(q.cfg.Dir, fmt.Sprintf("%s%d.jsonl", segmentPrefix, nano))
	if err := os.Rename(src, dst); err != nil {
		return err
	}

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

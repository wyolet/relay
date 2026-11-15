package lastused

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultFlushInterval is how often a Flusher writes what its Recorder noted.
const DefaultFlushInterval = time.Minute

// finalFlushTimeout bounds the flush Run makes once its context ends.
const finalFlushTimeout = 5 * time.Second

// Entry is one credential and the minute it last served.
type Entry struct {
	ID string
	At time.Time
}

// Writer stores last-used times, keeping a stored time that is later.
type Writer interface {
	TouchKeys(ctx context.Context, used []Entry) error
	TouchHostKeys(ctx context.Context, used []Entry) error
}

// Flusher writes a Recorder's entries through a Writer on an interval.
type Flusher struct {
	rec      *Recorder
	w        Writer
	interval time.Duration
	log      *slog.Logger

	// mu serializes Flush; keys and hostKeys are its reused buffers.
	mu             sync.Mutex
	keys, hostKeys []Entry
}

// NewFlusher returns a Flusher of rec's entries to w. interval <= 0 takes
// DefaultFlushInterval.
func NewFlusher(rec *Recorder, w Writer, interval time.Duration, log *slog.Logger) *Flusher {
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Flusher{rec: rec, w: w, interval: interval, log: log}
}

// Run flushes every interval until ctx ends, then flushes once more.
func (f *Flusher) Run(ctx context.Context) {
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
			f.flushLogged(final)
			cancel()
			return
		case <-t.C:
			f.flushLogged(ctx)
		}
	}
}

func (f *Flusher) flushLogged(ctx context.Context) {
	if err := f.Flush(ctx); err != nil {
		f.log.Warn("lastused: flush failed; retrying next interval", "err", err)
	}
}

// Flush writes everything noted since the last Flush, one statement per kind.
// Entries a write fails for are noted again for the next Flush.
func (f *Flusher) Flush(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys, f.hostKeys = f.keys[:0], f.hostKeys[:0]
	f.rec.Drain(func(keys, hostKeys map[string]int64) {
		f.keys = appendSorted(f.keys, keys)
		f.hostKeys = appendSorted(f.hostKeys, hostKeys)
	})
	var failedKeys, failedHostKeys []Entry
	var errs []error
	if len(f.keys) > 0 {
		if err := f.w.TouchKeys(ctx, f.keys); err != nil {
			failedKeys, errs = f.keys, append(errs, err)
		}
	}
	if len(f.hostKeys) > 0 {
		if err := f.w.TouchHostKeys(ctx, f.hostKeys); err != nil {
			failedHostKeys, errs = f.hostKeys, append(errs, err)
		}
	}
	if len(errs) > 0 {
		f.rec.restore(failedKeys, failedHostKeys)
	}
	return errors.Join(errs...)
}

// appendSorted appends m's entries to dst in id order, so pods writing the
// same rows at once lock them in the same order.
func appendSorted(dst []Entry, m map[string]int64) []Entry {
	start := len(dst)
	for id, minute := range m {
		dst = append(dst, Entry{ID: id, At: time.Unix(minute*60, 0).UTC()})
	}
	slices.SortFunc(dst[start:], func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	return dst
}

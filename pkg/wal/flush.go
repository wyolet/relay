package wal

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wyolet/relay/pkg/metrics"
)

// FlushPending finds all pending segments and flushes them oldest-first.
// Before flushing it enforces MaxSegments by dropping the oldest excess. A
// segment held in memory flushes from there, any other is read from its
// file. A segment whose flush fails is left on disk for the next pass.
func (q *Queue[T]) FlushPending() {
	q.flushMu.Lock()
	defer q.flushMu.Unlock()
	// Report after the pass so a failed flush (early return below) shows
	// as rising lag — the whole point of the gauge.
	defer q.reportFlushLag()

	segments, err := q.listSegments()
	if err != nil {
		q.log.Warn("wal: list segments", "err", err)
		return
	}

	// Enforce disk cap — drop oldest excess segments.
	if q.cfg.MaxSegments > 0 && len(segments) > q.cfg.MaxSegments {
		excess := segments[:len(segments)-q.cfg.MaxSegments]
		for _, s := range excess {
			lines, cerr := CountLines(s)
			if cerr != nil {
				q.log.Warn("wal: count lines of dropped segment", "file", filepath.Base(s), "err", cerr)
			}
			if rerr := os.Remove(s); rerr == nil {
				q.forgetSegment(s)
				q.log.Warn("wal: dropped oldest segment (maxSegments exceeded)", "file", filepath.Base(s), "records", lines)
				q.dropped.Add(1)
				metrics.AddRecordsLost(q.cfg.Kind, lines)
			}
		}
		segments = segments[len(segments)-q.cfg.MaxSegments:]
	}

	for _, seg := range segments {
		records, held, inMemory := q.takeRecords(seg)
		if !inMemory {
			var err error
			records, err = readSegment[T](seg, q.segmentLineCount(seg), q.log)
			if err != nil {
				q.log.Warn("wal: read segment", "file", filepath.Base(seg), "err", err)
				q.failing.Store(true)
				q.dropRetained()
				return
			}
		}
		err := q.flush(records)
		if inMemory {
			q.releaseRetained(held)
		}
		if err != nil {
			// Do NOT delete — leave for retry on next tick.
			q.log.Warn("wal: flush failed, will retry", "file", filepath.Base(seg), "err", err)
			q.failing.Store(true)
			q.dropRetained()
			return
		}
		if err := os.Remove(seg); err != nil {
			q.log.Warn("wal: remove segment", "file", filepath.Base(seg), "err", err)
		} else {
			q.forgetSegment(seg)
		}
	}
	q.failing.Store(false)
}

// segmentLineCount returns the line count recorded when this process rotated
// the segment, or 0 for one left by an earlier run.
func (q *Queue[T]) segmentLineCount(path string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.segments[path].lines
}

func (q *Queue[T]) forgetSegment(path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retainedBytes -= q.segments[path].bytes
	delete(q.segments, path)
}

// reportFlushLag publishes the age of the oldest un-flushed segment
// (relay_flush_lag_seconds{kind=...}) — "how stale is my data". Called under
// flushMu after every flush pass, so a sink outage shows as monotonically
// rising lag on each tick and a drained queue reads 0. The age comes from the
// unix-nano rotation timestamp in the filename — no stat calls.
func (q *Queue[T]) reportFlushLag() {
	segments, err := q.listSegments()
	if err != nil {
		return
	}
	var lag float64
	if len(segments) > 0 {
		if nano := segmentNano(segments[0]); nano > 0 {
			lag = time.Since(time.Unix(0, int64(nano))).Seconds()
		}
	}
	metrics.ReportFlushLag(q.cfg.Kind, lag)
}

// listSegments returns segment paths sorted oldest-first (by the embedded
// unix-nano timestamp in the filename).
func (q *Queue[T]) listSegments() ([]string, error) {
	entries, err := fs.Glob(os.DirFS(q.cfg.Dir), SegmentGlob)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, filepath.Join(q.cfg.Dir, e))
	}
	slices.SortFunc(paths, func(a, b string) int {
		return segmentNano(a) - segmentNano(b)
	})
	return paths, nil
}

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
// segment whose flush fails is left on disk for the next pass.
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
			if rerr := os.Remove(s); rerr == nil {
				q.log.Warn("wal: dropped oldest segment (maxSegments exceeded)", "file", filepath.Base(s))
				q.dropped.Add(1)
			}
		}
		segments = segments[len(segments)-q.cfg.MaxSegments:]
	}

	for _, seg := range segments {
		records, err := readSegment[T](seg, q.log)
		if err != nil {
			q.log.Warn("wal: read segment", "file", filepath.Base(seg), "err", err)
			return
		}
		if err := q.flush(records); err != nil {
			// Do NOT delete — leave for retry on next tick.
			q.log.Warn("wal: flush failed, will retry", "file", filepath.Base(seg), "err", err)
			return
		}
		if err := os.Remove(seg); err != nil {
			q.log.Warn("wal: remove segment", "file", filepath.Base(seg), "err", err)
		}
	}
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

package file

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/wyolet/relay/pkg/usage"
)

var _ usage.Reader = (*Reader)(nil)

// Reader is a usage.Reader backed by the same JSONL file Sink writes to.
// Linear scan per query — fine for dogfood files (MB range, thousands of
// events). For production-scale (millions of events, GB-scale files), use
// the ClickHouse backend instead.
type Reader struct {
	path string
}

// NewReader constructs a Reader for path. Path must match the Sink the
// relay is currently writing to.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// Events streams the file, applies filters, returns the newest matching
// events up to q.Limit.
func (r *Reader) Events(_ context.Context, q usage.EventQuery) ([]usage.Event, error) {
	all, err := r.scan()
	if err != nil {
		return nil, err
	}
	return usage.SortAndLimit(usage.FilterEvents(all, q), q.Limit), nil
}

// Summary streams the file, applies filters, builds per-group aggregates
// including latency percentiles.
func (r *Reader) Summary(_ context.Context, q usage.SummaryQuery) (usage.SummaryResult, error) {
	all, err := r.scan()
	if err != nil {
		return usage.SummaryResult{}, err
	}
	return usage.Summarize(usage.FilterEvents(all, q.EventQuery), q.GroupBy)
}

// TimeSeries streams the file, applies filters, and buckets the matching
// events into time series via usage.Bucketize.
func (r *Reader) TimeSeries(_ context.Context, q usage.TimeSeriesQuery) (usage.TimeSeriesResult, error) {
	all, err := r.scan()
	if err != nil {
		return usage.TimeSeriesResult{}, err
	}
	res, err := usage.Bucketize(usage.FilterEvents(all, q.EventQuery), q.Interval, q.GroupBy)
	if err != nil {
		return usage.TimeSeriesResult{}, err
	}
	res.Interval = q.Interval.String()
	return res, nil
}

// scan reads every event in the file (no filter/limit; callers evaluate
// via usage.FilterEvents). A missing file yields an empty slice, not an
// error — useful at boot before any request has fired.
func (r *Reader) scan() ([]usage.Event, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("usage/file.Reader: open %q: %w", r.path, err)
	}
	defer f.Close()

	var out []usage.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev usage.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Skip malformed lines silently — better to lose one event
			// than to fail the whole query.
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("usage/file.Reader: scan: %w", err)
	}
	return out, nil
}

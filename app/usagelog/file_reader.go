package usagelog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// FileReader is a Reader backed by the same JSONL file FileSink writes
// to. Linear scan per query — fine for dogfood files (MB range,
// thousands of events). For production-scale (millions of events,
// GB-scale files), swap to a ClickHouseReader or similar.
type FileReader struct {
	path string
}

// NewFileReader constructs a FileReader for path. Path must match the
// FileSink the relay is currently writing to.
func NewFileReader(path string) *FileReader {
	return &FileReader{path: path}
}

// Events streams the file, applies filters, returns the newest matching
// events up to q.Limit. Sort by timestamp descending.
func (r *FileReader) Events(_ context.Context, q EventQuery) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}

	matches, err := r.scan(q)
	if err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Timestamp.After(matches[j].Timestamp)
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// Summary streams the file, applies filters, builds per-group
// aggregates including latency percentiles.
func (r *FileReader) Summary(_ context.Context, q SummaryQuery) (SummaryResult, error) {
	groupBy := q.GroupBy
	if groupBy == "" {
		groupBy = "source"
	}
	if !IsValidGroupBy(groupBy) {
		return SummaryResult{}, fmt.Errorf("usagelog.Summary: invalid group_by %q", groupBy)
	}

	events, err := r.scan(q.EventQuery)
	if err != nil {
		return SummaryResult{}, err
	}

	type bucket struct {
		count, errs int64
		tokens      map[string]int64
		latencies   []int64
		first, last time.Time
	}
	groups := map[string]*bucket{}
	from, to := time.Time{}, time.Time{}

	for _, ev := range events {
		key := groupKey(ev, groupBy)
		b, ok := groups[key]
		if !ok {
			b = &bucket{tokens: map[string]int64{}, first: ev.Timestamp, last: ev.Timestamp}
			groups[key] = b
		}
		b.count++
		if ev.Status >= 400 {
			b.errs++
		}
		for k, v := range ev.Tokens {
			b.tokens[k] += v
		}
		b.latencies = append(b.latencies, ev.DurationMs)
		if ev.Timestamp.Before(b.first) {
			b.first = ev.Timestamp
		}
		if ev.Timestamp.After(b.last) {
			b.last = ev.Timestamp
		}
		if from.IsZero() || ev.Timestamp.Before(from) {
			from = ev.Timestamp
		}
		if ev.Timestamp.After(to) {
			to = ev.Timestamp
		}
	}

	rows := make([]SummaryRow, 0, len(groups))
	for key, b := range groups {
		rows = append(rows, SummaryRow{
			Group:      map[string]string{groupBy: key},
			Requests:   b.count,
			ErrorCount: b.errs,
			Tokens:     b.tokens,
			DurationMs: durationStats(b.latencies),
			FirstSeen:  b.first,
			LastSeen:   b.last,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Requests > rows[j].Requests
	})

	return SummaryResult{Rows: rows, From: from, To: to}, nil
}

// scan opens the file once and returns every event matching the
// filter (no limit applied at this layer; callers cap).
func (r *FileReader) scan(q EventQuery) ([]Event, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("usagelog.FileReader: open %q: %w", r.path, err)
	}
	defer f.Close()

	var cutoff time.Time
	if q.Since > 0 {
		cutoff = time.Now().Add(-q.Since)
	}

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Skip malformed lines silently — better to lose one event
			// than to fail the whole query.
			continue
		}
		if !matches(ev, q, cutoff) {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("usagelog.FileReader: scan: %w", err)
	}
	return out, nil
}

func matches(ev Event, q EventQuery, cutoff time.Time) bool {
	if !cutoff.IsZero() && ev.Timestamp.Before(cutoff) {
		return false
	}
	if q.RelayKeyHash != "" && ev.RelayKeyHash != q.RelayKeyHash {
		return false
	}
	if q.PolicyID != "" && ev.PolicyID != q.PolicyID {
		return false
	}
	if q.ModelID != "" && ev.ModelID != q.ModelID {
		return false
	}
	if q.HostID != "" && ev.HostID != q.HostID {
		return false
	}
	if q.Source != "" && ev.Source != q.Source {
		return false
	}
	if q.StatusMin > 0 && ev.Status < q.StatusMin {
		return false
	}
	if q.StatusMax > 0 && ev.Status > q.StatusMax {
		return false
	}
	return true
}

func groupKey(ev Event, groupBy string) string {
	switch groupBy {
	case "relay_key_hash":
		return ev.RelayKeyHash
	case "policy_id":
		return ev.PolicyID
	case "model_id":
		return ev.ModelID
	case "host_id":
		return ev.HostID
	case "host_key_id":
		return ev.HostKeyID
	default: // "source"
		return ev.Source
	}
}

// durationStats computes avg + percentiles + max from raw samples.
// For dogfood-sized inputs (thousands of values per group) a full
// sort is fine. Production scale would use t-digest or HDR histogram.
func durationStats(samples []int64) DurationStats {
	if len(samples) == 0 {
		return DurationStats{}
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return DurationStats{
		Avg: sum / int64(len(sorted)),
		P50: percentile(sorted, 0.50),
		P95: percentile(sorted, 0.95),
		P99: percentile(sorted, 0.99),
		Max: sorted[len(sorted)-1],
	}
}

func percentile(sortedAsc []int64, p float64) int64 {
	if len(sortedAsc) == 0 {
		return 0
	}
	idx := int(float64(len(sortedAsc)-1) * p)
	return sortedAsc[idx]
}

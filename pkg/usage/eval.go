package usage

import (
	"fmt"
	"sort"
	"time"
)

// In-memory query evaluation shared by scan-style backends (file, valkey)
// that pull a candidate set of events and filter/aggregate in Go rather
// than pushing the query into a store engine. The ClickHouse backend does
// NOT use these — it pushes filters + aggregation into SQL.

// FilterEvents returns the subset of events matching q's time cutoff,
// dimension filters, and status range. It does not sort or apply Limit;
// use SortAndLimit for that.
func FilterEvents(events []Event, q EventQuery) []Event {
	var cutoff time.Time
	if q.Since > 0 {
		cutoff = time.Now().Add(-q.Since)
	}
	out := events[:0:0]
	for _, ev := range events {
		if matches(ev, q, cutoff) {
			out = append(out, ev)
		}
	}
	return out
}

// SortAndLimit sorts events newest-first and caps to limit (clamped to
// [DefaultEventLimit, MaxEventLimit] when out of range).
func SortAndLimit(events []Event, limit int) []Event {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.After(events[j].Timestamp)
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events
}

// Summarize groups the (already-filtered) events by groupBy and builds
// per-group totals + latency percentiles, sorted by request count desc.
// Returns an error for an unknown groupBy dimension.
func Summarize(events []Event, groupBy string) (SummaryResult, error) {
	if groupBy == "" {
		groupBy = "source"
	}
	if !IsValidGroupBy(groupBy) {
		return SummaryResult{}, fmt.Errorf("usage.Summarize: invalid group_by %q", groupBy)
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

// durationStats computes avg + percentiles + max from raw samples. For
// dogfood-sized inputs (thousands of values per group) a full sort is
// fine. Production scale would use t-digest or HDR histogram.
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

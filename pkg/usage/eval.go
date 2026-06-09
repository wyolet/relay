package usage

import (
	"fmt"
	"sort"
	"strings"
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
	switch {
	case !q.From.IsZero():
		cutoff = q.From
	case q.Since > 0:
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
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.After(events[j].Timestamp)
		}
		return events[i].RequestID > events[j].RequestID // stable keyset tiebreak
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events
}

// Summarize groups the (already-filtered) events by groupBy and builds
// per-group totals + latency percentiles, sorted by request count desc.
// LogOnly events (pre-upstream rejections) are skipped — they belong to
// the logs view, not usage stats. Returns an error for an unknown groupBy
// dimension.
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
		if ev.LogOnly() {
			continue
		}
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

// Bucketize groups the (already-filtered) events into time buckets of
// width interval, optionally split into one series per groupBy dimension.
// Buckets align to the Unix epoch (bucketStart = floor(unix/intervalSec));
// empty buckets are omitted. Points within a series are ordered
// oldest-first; series (rows) are ordered by total request count desc.
// Returns an error for a non-positive interval or unknown groupBy.
func Bucketize(events []Event, interval time.Duration, groupBy string) (TimeSeriesResult, error) {
	if interval <= 0 {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: interval must be > 0")
	}
	if groupBy != "" && !IsValidGroupBy(groupBy) {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: invalid group_by %q", groupBy)
	}
	intervalSec := int64(interval / time.Second)
	if intervalSec <= 0 {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: interval must be >= 1s")
	}

	type point struct {
		requests, errs int64
		tokens         map[string]int64
	}
	// seriesKey -> bucketUnix -> point
	series := map[string]map[int64]*point{}
	totals := map[string]int64{}
	from, to := time.Time{}, time.Time{}

	for _, ev := range events {
		if ev.LogOnly() {
			continue
		}
		sk := ""
		if groupBy != "" {
			sk = groupKey(ev, groupBy)
		}
		buckets, ok := series[sk]
		if !ok {
			buckets = map[int64]*point{}
			series[sk] = buckets
		}
		bu := ev.Timestamp.Unix() - (ev.Timestamp.Unix() % intervalSec)
		p, ok := buckets[bu]
		if !ok {
			p = &point{tokens: map[string]int64{}}
			buckets[bu] = p
		}
		p.requests++
		totals[sk]++
		if ev.Status >= 400 {
			p.errs++
		}
		for k, v := range ev.Tokens {
			p.tokens[k] += v
		}
		if from.IsZero() || ev.Timestamp.Before(from) {
			from = ev.Timestamp
		}
		if ev.Timestamp.After(to) {
			to = ev.Timestamp
		}
	}

	rows := make([]TimeSeriesRow, 0, len(series))
	for sk, buckets := range series {
		bus := make([]int64, 0, len(buckets))
		for bu := range buckets {
			bus = append(bus, bu)
		}
		sort.Slice(bus, func(i, j int) bool { return bus[i] < bus[j] })

		points := make([]TimeSeriesPoint, 0, len(bus))
		for _, bu := range bus {
			p := buckets[bu]
			points = append(points, TimeSeriesPoint{
				Bucket:     time.Unix(bu, 0).UTC(),
				Requests:   p.requests,
				ErrorCount: p.errs,
				Tokens:     p.tokens,
			})
		}
		row := TimeSeriesRow{Points: points}
		if groupBy != "" {
			row.Group = map[string]string{groupBy: sk}
		}
		rows = append(rows, row)
	}
	SortTimeSeriesRows(rows, totals, groupBy)

	return TimeSeriesResult{Rows: rows, From: from, To: to}, nil
}

// SortTimeSeriesRows orders series by total request count desc, with the
// group value as a stable tiebreak. totals is keyed by the series key
// (the groupBy dimension value, or "" for the single-series case). Shared
// by the in-memory Bucketize path and the SQL backends so all readers
// return series in the same order.
func SortTimeSeriesRows(rows []TimeSeriesRow, totals map[string]int64, groupBy string) {
	key := func(r TimeSeriesRow) string {
		if groupBy != "" && r.Group != nil {
			return r.Group[groupBy]
		}
		return ""
	}
	sort.Slice(rows, func(i, j int) bool {
		ki, kj := key(rows[i]), key(rows[j])
		if totals[ki] != totals[kj] {
			return totals[ki] > totals[kj]
		}
		return ki < kj
	})
}

func matches(ev Event, q EventQuery, cutoff time.Time) bool {
	// Lower bound: cutoff (From, or now-Since — resolved by caller).
	if !cutoff.IsZero() && ev.Timestamp.Before(cutoff) {
		return false
	}
	if !q.To.IsZero() && ev.Timestamp.After(q.To) {
		return false
	}
	// Keyset cursor: strictly older than (CursorTS, CursorID) under
	// (ts DESC, request_id DESC).
	if !q.CursorTS.IsZero() {
		if ev.Timestamp.After(q.CursorTS) {
			return false
		}
		if ev.Timestamp.Equal(q.CursorTS) && ev.RequestID >= q.CursorID {
			return false
		}
	}
	if q.RequestID != "" && ev.RequestID != q.RequestID {
		return false
	}
	if !inList(q.RelayKeyHash, ev.RelayKeyHash) {
		return false
	}
	if !inList(q.PolicyID, ev.PolicyID) {
		return false
	}
	if !inList(q.ModelID, ev.ModelID) {
		return false
	}
	if !inList(q.HostID, ev.HostID) {
		return false
	}
	if !inList(q.Source, ev.Source) {
		return false
	}
	if !inList(q.FinishReason, ev.FinishReason) {
		return false
	}
	if !inList(q.ErrorKind, ev.ErrorKind) {
		return false
	}
	if q.StatusMin > 0 && ev.Status < q.StatusMin {
		return false
	}
	if q.StatusMax > 0 && ev.Status > q.StatusMax {
		return false
	}
	if len(q.Status) > 0 && !intInList(q.Status, ev.Status) {
		return false
	}
	if !inList(q.HostKeyID, ev.HostKeyID) {
		return false
	}
	if !inList(q.RequestedModel, ev.RequestedModel) {
		return false
	}
	if q.Streamed != nil && ev.Streamed != *q.Streamed {
		return false
	}
	if q.ErrorsOnly != nil {
		isErr := ev.Status >= 400 || ev.ErrorKind != ""
		if isErr != *q.ErrorsOnly {
			return false
		}
	}
	if q.AttemptsMin > 0 && ev.Attempts < q.AttemptsMin {
		return false
	}
	if q.DurationMsMin > 0 && ev.DurationMs < q.DurationMsMin {
		return false
	}
	if q.DurationMsMax > 0 && ev.DurationMs > q.DurationMsMax {
		return false
	}
	if q.TTFTMsMin > 0 || q.TTFTMsMax > 0 {
		if ev.Upstream == nil {
			return false
		}
		ttft := ev.Upstream.ResponseStart / 1000 // µs → ms
		if q.TTFTMsMin > 0 && ttft < q.TTFTMsMin {
			return false
		}
		if q.TTFTMsMax > 0 && ttft > q.TTFTMsMax {
			return false
		}
	}
	if q.Q != "" {
		needle := strings.ToLower(q.Q)
		if !containsFold(ev.RequestID, needle) &&
			!containsFold(ev.ModelID, needle) &&
			!containsFold(ev.RequestedModel, needle) &&
			!containsFold(ev.Source, needle) {
			return false
		}
	}
	return true
}

func intInList(set []int, v int) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// containsFold reports whether s contains needle (needle already lowercased).
func containsFold(s, lowerNeedle string) bool {
	return strings.Contains(strings.ToLower(s), lowerNeedle)
}

// inList reports whether val is in set, treating an empty set as "no
// filter" (matches everything).
func inList(set []string, val string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == val {
			return true
		}
	}
	return false
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

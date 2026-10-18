package usage

import (
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Summarize groups the (already-filtered) events by groupBy and builds
// per-group totals + latency percentiles, sorted by request count desc.
// LogOnly events (pre-upstream rejections) are skipped — they belong to
// the logs view, not usage stats. Returns an error for an unknown groupBy
// dimension.
func Summarize(events []Event, groupBy string) (SummaryResult, error) {
	return SummarizeWithDistinct(events, groupBy, "")
}

// SummarizeWithDistinct is Summarize plus SummaryRow.Distinct over the distinct dimension (a ValidDistinct entry; "" for none).
func SummarizeWithDistinct(events []Event, groupBy, distinct string) (SummaryResult, error) {
	if groupBy == "" {
		groupBy = "source"
	}
	if !IsValidGroupBy(groupBy) {
		return SummaryResult{}, fmt.Errorf("usage.Summarize: invalid group_by %q", groupBy)
	}
	if _, ok := DistinctColumn(distinct); distinct != "" && !ok {
		return SummaryResult{}, fmt.Errorf("usage.Summarize: invalid distinct %q", distinct)
	}

	type bucket struct {
		count          int64
		errs           errorCounts
		tokens         map[string]int64
		latencies      []int64
		ttfts          []int64
		first, last    time.Time
		cost, unpriced int64
		distinct       distinctValues
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
			b = &bucket{tokens: map[string]int64{}, first: ev.Timestamp, last: ev.Timestamp, distinct: newDistinctValues(distinct)}
			groups[key] = b
		}
		b.count++
		b.distinct.add(ev, distinct)
		b.errs.add(ev.Status)
		for k, v := range ev.Tokens {
			b.tokens[k] += v
		}
		b.latencies = append(b.latencies, ev.DurationMs)
		if ttft, ok := ttftUs(ev); ok {
			b.ttfts = append(b.ttfts, ttft)
		}
		if ev.CostNanos != nil {
			b.cost += *ev.CostNanos
		} else {
			b.unpriced++
		}
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
			Group:      GroupLabel(groupBy, key),
			Requests:   b.count,
			ErrorCount: b.errs.total,
			Errors4xx:  b.errs.c4xx,
			Errors5xx:  b.errs.c5xx,
			Throttled:  b.errs.throttled,
			Tokens:     b.tokens,
			DurationMs: durationStats(b.latencies),
			TTFTMs:     ttftStats(b.ttfts),
			FirstSeen:  b.first,
			LastSeen:   b.last,
			CostNanos:  b.cost,
			Unpriced:   b.unpriced,
			Distinct:   b.distinct.count(),
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
	return BucketizeWithDistinct(events, interval, groupBy, "")
}

// BucketizeWithDistinct is Bucketize plus TimeSeriesPoint.Distinct over the distinct dimension (a ValidDistinct entry; "" for none).
func BucketizeWithDistinct(events []Event, interval time.Duration, groupBy, distinct string) (TimeSeriesResult, error) {
	if interval <= 0 {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: interval must be > 0")
	}
	if groupBy != "" && !IsValidGroupBy(groupBy) {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: invalid group_by %q", groupBy)
	}
	if _, ok := DistinctColumn(distinct); distinct != "" && !ok {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: invalid distinct %q", distinct)
	}
	intervalSec := int64(interval / time.Second)
	if intervalSec <= 0 {
		return TimeSeriesResult{}, fmt.Errorf("usage.Bucketize: interval must be >= 1s")
	}

	type point struct {
		requests       int64
		errs           errorCounts
		tokens         map[string]int64
		latencies      []int64
		ttfts          []int64
		cost, unpriced int64
		distinct       distinctValues
	}
	// seriesKey -> bucketUnix -> point
	series := map[string]map[int64]*point{}
	totals := map[string]int64{}
	from, to := time.Time{}, time.Time{}

	for _, ev := range events {
		if ev.LogOnly() {
			continue
		}
		sk := groupKey(ev, groupBy)
		buckets, ok := series[sk]
		if !ok {
			buckets = map[int64]*point{}
			series[sk] = buckets
		}
		bu := ev.Timestamp.Unix() - (ev.Timestamp.Unix() % intervalSec)
		p, ok := buckets[bu]
		if !ok {
			p = &point{tokens: map[string]int64{}, distinct: newDistinctValues(distinct)}
			buckets[bu] = p
		}
		p.requests++
		p.distinct.add(ev, distinct)
		totals[sk]++
		p.errs.add(ev.Status)
		for k, v := range ev.Tokens {
			p.tokens[k] += v
		}
		p.latencies = append(p.latencies, ev.DurationMs)
		if ttft, ok := ttftUs(ev); ok {
			p.ttfts = append(p.ttfts, ttft)
		}
		if ev.CostNanos != nil {
			p.cost += *ev.CostNanos
		} else {
			p.unpriced++
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
				ErrorCount: p.errs.total,
				Errors4xx:  p.errs.c4xx,
				Errors5xx:  p.errs.c5xx,
				Throttled:  p.errs.throttled,
				Tokens:     p.tokens,
				DurationMs: durationStats(p.latencies),
				TTFTMs:     ttftStats(p.ttfts),
				CostNanos:  p.cost,
				Unpriced:   p.unpriced,
				Distinct:   p.distinct.count(),
			})
		}
		rows = append(rows, TimeSeriesRow{Group: GroupLabel(groupBy, sk), Points: points})
	}
	SortTimeSeriesRows(rows, totals, groupBy)

	return TimeSeriesResult{Rows: rows, From: from, To: to}, nil
}

// errorCounts tallies ErrorCount and its status-class split for one row (invariant on SummaryRow).
type errorCounts struct {
	total, c4xx, c5xx, throttled int64
}

func (c *errorCounts) add(status int) {
	if status < 400 {
		return
	}
	c.total++
	if status < 500 {
		c.c4xx++
	} else {
		c.c5xx++
	}
	if status == http.StatusTooManyRequests {
		c.throttled++
	}
}

// SortTimeSeriesRows orders series by total request count desc, with the
// group value as a stable tiebreak. totals is keyed by the series key
// (the groupBy dimension value, or "" for the single-series case). Shared
// by the in-memory Bucketize path and the SQL backends so all readers
// return series in the same order.
func SortTimeSeriesRows(rows []TimeSeriesRow, totals map[string]int64, groupBy string) {
	key := func(r TimeSeriesRow) string {
		if Grouped(groupBy) && r.Group != nil {
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

package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// TimeSeries buckets matched events by q.Interval (epoch-aligned via
// toStartOfInterval), optionally split into one series per q.GroupBy
// value. Empty buckets are not emitted. Series are ordered by total
// requests desc; points within a series oldest-first.
func (s *Sink) TimeSeries(ctx context.Context, q usage.TimeSeriesQuery) (usage.TimeSeriesResult, error) {
	if q.Interval <= 0 {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/clickhouse: interval must be > 0")
	}
	intervalSec := int64(q.Interval / time.Second)
	if intervalSec <= 0 {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/clickhouse: interval must be >= 1s")
	}
	groupBy := q.GroupBy
	if groupBy != "" && !usage.IsValidGroupBy(groupBy) {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/clickhouse: invalid groupBy %q", groupBy)
	}
	selDistinct, err := distinctSelect(q.Distinct)
	if err != nil {
		return usage.TimeSeriesResult{}, err
	}

	where, args := buildWhere(q.EventQuery, true)

	bucketExpr := fmt.Sprintf("toStartOfInterval(ts, INTERVAL %d second)", intervalSec)
	selCols := bucketExpr + " AS bucket"
	groupOrder := "bucket"
	grouped, labeled := usage.Grouped(groupBy), false
	if grouped {
		grpExpr := groupBy
		if column, key, ok := usage.MapGroupKey(groupBy); ok {
			grpExpr = column + "[?]"
			// clickhouse-go substitutes ? placeholders in text order — the
			// group key appears in the SELECT list, ahead of the WHERE args.
			args = append([]any{key}, args...)
		}
		// Each bucket reports its latest slug and when it was seen; the newest across a series' buckets is the series label.
		if labelExpr, labelTSExpr, ok := latestLabelSQL(groupBy); ok {
			grpExpr += " AS grp, " + labelExpr + " AS lbl, " + labelTSExpr + " AS lbl_ts"
			labeled = true
		} else {
			grpExpr += " AS grp"
		}
		selCols = grpExpr + ", " + selCols
		groupOrder = "grp, bucket"
	}

	sql := fmt.Sprintf(`
SELECT
    %s,
    toInt64(count())                AS requests,
    `+errorSelectSQL+`,
    sumMap(tokens)                  AS tokens,
    toInt64(avg(duration_ms))           AS avg_ms,
    toInt64(quantile(0.5)(duration_ms)) AS p50,
    toInt64(quantile(0.95)(duration_ms)) AS p95,
    toInt64(quantile(0.99)(duration_ms)) AS p99,
    max(duration_ms)                AS max_ms,
    `+ttftSelectSQL+`,
    `+costSelectSQL+`,
    min(ts)                         AS first_seen,
    max(ts)                         AS last_seen%s
FROM %s%s
GROUP BY %s
ORDER BY %s`,
		selCols, selDistinct, chTable, where, groupOrder, groupOrder)

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/clickhouse: timeseries query: %w", err)
	}
	defer rows.Close()

	res := usage.TimeSeriesResult{Interval: q.Interval.String()}
	byKey := map[string]int{} // series key -> index into res.Rows
	totals := map[string]int64{}
	var labels usage.LatestLabels
	for rows.Next() {
		var (
			grp, label           string
			labelTS              time.Time
			bucket               time.Time
			requests             int64
			errs                 errorCols
			tokens               map[string]int64
			avgMs                int64
			p50, p95, p99, maxMs int64
			ttft                 ttftCols
			costNanos, unpriced  int64
			first                time.Time
			last                 time.Time
			distinct             int64
		)
		dest := []any{&bucket, &requests, &errs.total, &errs.c4xx, &errs.c5xx, &errs.throttled, &tokens,
			&avgMs, &p50, &p95, &p99, &maxMs,
			&ttft.count, &ttft.avg, &ttft.p50, &ttft.p95, &ttft.p99, &ttft.max,
			&costNanos, &unpriced,
			&first, &last}
		switch {
		case labeled:
			dest = append([]any{&grp, &label, &labelTS}, dest...)
		case grouped:
			dest = append([]any{&grp}, dest...)
		}
		if selDistinct != "" {
			dest = append(dest, &distinct)
		}
		if err := rows.Scan(dest...); err != nil {
			return usage.TimeSeriesResult{}, fmt.Errorf("usage/clickhouse: scan timeseries: %w", err)
		}
		labels.Add(grp, label, labelTS)
		idx, ok := byKey[grp]
		if !ok {
			res.Rows = append(res.Rows, usage.TimeSeriesRow{Group: usage.GroupLabel(groupBy, grp)})
			idx = len(res.Rows) - 1
			byKey[grp] = idx
		}
		res.Rows[idx].Points = append(res.Rows[idx].Points, usage.TimeSeriesPoint{
			Bucket:     bucket.UTC(),
			Requests:   requests,
			ErrorCount: errs.total,
			Errors4xx:  errs.c4xx,
			Errors5xx:  errs.c5xx,
			Throttled:  errs.throttled,
			Tokens:     tokens,
			DurationMs: usage.DurationStats{Avg: avgMs, P50: p50, P95: p95, P99: p99, Max: maxMs},
			TTFTMs:     ttft.stats(),
			CostNanos:  costNanos,
			Unpriced:   unpriced,
			Distinct:   distinctCount(q.Distinct, distinct),
		})
		totals[grp] += requests
		if res.From.IsZero() || first.Before(res.From) {
			res.From = first
		}
		if last.After(res.To) {
			res.To = last
		}
	}
	if err := rows.Err(); err != nil {
		return usage.TimeSeriesResult{}, err
	}

	usage.SortTimeSeriesRows(res.Rows, totals, groupBy)
	res.Labels = labels.Map()
	return res, nil
}

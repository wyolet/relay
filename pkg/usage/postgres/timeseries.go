package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// TimeSeries buckets matched events by q.Interval (date_bin, epoch-aligned),
// optionally split into one series per q.GroupBy value. Like Summary, token
// sums come from a second jsonb_each_text query merged in Go. Empty buckets
// are not emitted; series are ordered by total requests desc, points
// oldest-first.
func (s *Sink) TimeSeries(ctx context.Context, q usage.TimeSeriesQuery) (usage.TimeSeriesResult, error) {
	if q.Interval <= 0 {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: interval must be > 0")
	}
	intervalSec := int64(q.Interval / time.Second)
	if intervalSec <= 0 {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: interval must be >= 1s")
	}
	groupBy := q.GroupBy
	if groupBy != "" && !usage.IsValidGroupBy(groupBy) {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: invalid groupBy %q", groupBy)
	}

	where, args := buildWhere(q.EventQuery, true)
	bucketExpr := fmt.Sprintf("date_bin(interval '%d seconds', ts, TIMESTAMPTZ 'epoch')", intervalSec)

	grouped := usage.Grouped(groupBy)
	selPrefix, grpCols, subGrp := "", "", ""
	if grouped {
		var grpExpr string
		grpExpr, args = groupExpr(groupBy, args)
		selPrefix = grpExpr + " AS grp, "
		grpCols = "grp, "
		subGrp = "sub.grp, "
	}

	scalarSQL := fmt.Sprintf(`
SELECT
    %s%s AS bucket,
    count(*)::bigint                              AS requests,
    `+errorSelectSQL+`,
    trunc(avg(duration_ms))::bigint               AS avg_ms,
    (percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms))::bigint  AS p50,
    (percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms))::bigint AS p95,
    (percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms))::bigint AS p99,
    max(duration_ms)                              AS max_ms,
    `+ttftSelectSQL+`,
    `+costSelectSQL+`,
    min(ts)                                       AS first_seen,
    max(ts)                                       AS last_seen
FROM %s%s
GROUP BY %sbucket
ORDER BY %sbucket`,
		selPrefix, bucketExpr, s.cfg.Table, where, grpCols, grpCols)

	scalarRows, err := s.pool.Query(ctx, scalarSQL, args...)
	if err != nil {
		return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: timeseries scalar query: %w", err)
	}
	defer scalarRows.Close()

	res := usage.TimeSeriesResult{Interval: q.Interval.String()}
	rowIdx := map[string]int{}   // series key -> index into res.Rows
	pointIdx := map[string]int{} // grp|bucketUnix -> index into row.Points
	totals := map[string]int64{}
	pkey := func(grp string, bu int64) string { return grp + "|" + strconv.FormatInt(bu, 10) }

	for scalarRows.Next() {
		var (
			grp                  string
			bucket               time.Time
			requests             int64
			errs                 errorCols
			avgMs                int64
			p50, p95, p99, maxMs int64
			ttft                 ttftCols
			costNanos, unpriced  int64
			first, last          time.Time
		)
		dest := []any{&bucket, &requests, &errs.total, &errs.c4xx, &errs.c5xx, &errs.throttled,
			&avgMs, &p50, &p95, &p99, &maxMs,
			&ttft.avg, &ttft.p50, &ttft.p95, &ttft.p99, &ttft.max,
			&costNanos, &unpriced,
			&first, &last}
		if grouped {
			dest = append([]any{&grp}, dest...)
		}
		if err := scalarRows.Scan(dest...); err != nil {
			return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: scan timeseries scalar: %w", err)
		}
		ri, ok := rowIdx[grp]
		if !ok {
			res.Rows = append(res.Rows, usage.TimeSeriesRow{Group: usage.GroupLabel(groupBy, grp)})
			ri = len(res.Rows) - 1
			rowIdx[grp] = ri
		}
		res.Rows[ri].Points = append(res.Rows[ri].Points, usage.TimeSeriesPoint{
			Bucket:     bucket.UTC(),
			Requests:   requests,
			ErrorCount: errs.total,
			Errors4xx:  errs.c4xx,
			Errors5xx:  errs.c5xx,
			Throttled:  errs.throttled,
			Tokens:     map[string]int64{},
			DurationMs: usage.DurationStats{Avg: avgMs, P50: p50, P95: p95, P99: p99, Max: maxMs},
			TTFTMs:     ttft.stats(),
			CostNanos:  costNanos,
			Unpriced:   unpriced,
		})
		pointIdx[pkey(grp, bucket.Unix())] = len(res.Rows[ri].Points) - 1
		totals[grp] += requests
		if res.From.IsZero() || first.Before(res.From) {
			res.From = first
		}
		if last.After(res.To) {
			res.To = last
		}
	}
	if err := scalarRows.Err(); err != nil {
		return usage.TimeSeriesResult{}, err
	}

	if len(res.Rows) > 0 {
		tokenSQL := fmt.Sprintf(`
SELECT %ssub.bucket, kv.key, sum(kv.value::bigint)::bigint
FROM (SELECT %s%s AS bucket, tokens FROM %s%s) AS sub
CROSS JOIN LATERAL jsonb_each_text(sub.tokens) AS kv
GROUP BY %ssub.bucket, kv.key`,
			subGrp, selPrefix, bucketExpr, s.cfg.Table, where, subGrp,
		)
		tokenRows, err := s.pool.Query(ctx, tokenSQL, args...)
		if err != nil {
			return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: timeseries token query: %w", err)
		}
		defer tokenRows.Close()

		for tokenRows.Next() {
			var (
				grp    string
				bucket time.Time
				key    string
				val    int64
			)
			dest := []any{&bucket, &key, &val}
			if grouped {
				dest = append([]any{&grp}, dest...)
			}
			if err := tokenRows.Scan(dest...); err != nil {
				return usage.TimeSeriesResult{}, fmt.Errorf("usage/postgres: scan timeseries token: %w", err)
			}
			ri, ok := rowIdx[grp]
			if !ok {
				continue
			}
			pi, ok := pointIdx[pkey(grp, bucket.Unix())]
			if !ok {
				continue
			}
			res.Rows[ri].Points[pi].Tokens[key] = val
		}
		if err := tokenRows.Err(); err != nil {
			return usage.TimeSeriesResult{}, err
		}
	}

	usage.SortTimeSeriesRows(res.Rows, totals, groupBy)
	return res, nil
}

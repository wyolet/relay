package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// Summary returns aggregated rows grouped by q.GroupBy.
func (s *Sink) Summary(ctx context.Context, q usage.SummaryQuery) (usage.SummaryResult, error) {
	groupBy := q.GroupBy
	if groupBy == "" {
		groupBy = "source"
	}
	if !usage.IsValidGroupBy(groupBy) {
		return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: invalid groupBy %q", groupBy)
	}

	grouped := usage.Grouped(groupBy)
	selGroup, groupClause := "", ungroupedHaving
	var preArgs []any
	if grouped {
		grpExpr := groupBy // column name matches the GroupBy value in this schema
		if column, key, ok := usage.MapGroupKey(groupBy); ok {
			// <map>[?] binds the key (never spliced into SQL text); a missing
			// key yields '' (Map default), matching the other backends.
			grpExpr = column + "[?]"
			preArgs = []any{key}
		}
		selGroup, groupClause = grpExpr+" AS grp,", "GROUP BY grp"
	}

	where, args := buildWhere(q.EventQuery, true)
	// clickhouse-go substitutes ? placeholders in text order — the group
	// key appears in the SELECT list, ahead of the WHERE args.
	args = append(preArgs, args...)

	sql := fmt.Sprintf(`
SELECT
    %s
    toInt64(count())                    AS requests,
    `+errorSelectSQL+`,
    sumMap(tokens)                      AS tokens,
    toInt64(avg(duration_ms))           AS avg_ms,
    toInt64(quantile(0.5)(duration_ms)) AS p50,
    toInt64(quantile(0.95)(duration_ms)) AS p95,
    toInt64(quantile(0.99)(duration_ms)) AS p99,
    max(duration_ms)                    AS max_ms,
    `+ttftSelectSQL+`,
    `+costSelectSQL+`,
    min(ts)                             AS first_seen,
    max(ts)                             AS last_seen
FROM %s%s
%s
ORDER BY requests DESC`,
		selGroup, chTable, where, groupClause)

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: summary query: %w", err)
	}
	defer rows.Close()

	var result usage.SummaryResult
	for rows.Next() {
		var (
			groupVal             string
			requests             int64
			errs                 errorCols
			tokens               map[string]int64
			avgMs                int64
			p50, p95, p99, maxMs int64
			ttft                 ttftCols
			costNanos, unpriced  int64
			firstSeen, lastSeen  time.Time
		)
		dest := []any{&requests, &errs.total, &errs.c4xx, &errs.c5xx, &errs.throttled,
			&tokens, &avgMs, &p50, &p95, &p99, &maxMs,
			&ttft.count, &ttft.avg, &ttft.p50, &ttft.p95, &ttft.p99, &ttft.max,
			&costNanos, &unpriced,
			&firstSeen, &lastSeen}
		if grouped {
			dest = append([]any{&groupVal}, dest...)
		}
		if err := rows.Scan(dest...); err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: scan summary: %w", err)
		}
		result.Rows = append(result.Rows, usage.SummaryRow{
			Group:      usage.GroupLabel(groupBy, groupVal),
			Requests:   requests,
			ErrorCount: errs.total,
			Errors4xx:  errs.c4xx,
			Errors5xx:  errs.c5xx,
			Throttled:  errs.throttled,
			Tokens:     tokens,
			DurationMs: usage.DurationStats{
				Avg: avgMs,
				P50: p50,
				P95: p95,
				P99: p99,
				Max: maxMs,
			},
			TTFTMs:    ttft.stats(),
			FirstSeen: firstSeen,
			LastSeen:  lastSeen,
			CostNanos: costNanos,
			Unpriced:  unpriced,
		})
		if result.From.IsZero() || firstSeen.Before(result.From) {
			result.From = firstSeen
		}
		if lastSeen.After(result.To) {
			result.To = lastSeen
		}
	}
	return result, rows.Err()
}

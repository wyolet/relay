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

	grpExpr := groupBy // column name matches the GroupBy value in this schema
	var preArgs []any
	if column, key, ok := usage.MapGroupKey(groupBy); ok {
		// <map>[?] binds the key (never spliced into SQL text); a missing
		// key yields '' (Map default), matching the other backends.
		grpExpr = column + "[?]"
		preArgs = []any{key}
	}

	where, args := buildWhere(q.EventQuery, true)
	// clickhouse-go substitutes ? placeholders in text order — the group
	// key appears in the SELECT list, ahead of the WHERE args.
	args = append(preArgs, args...)

	sql := fmt.Sprintf(`
SELECT
    %s AS grp,
    toInt64(count())                    AS requests,
    toInt64(countIf(status >= 400))     AS error_count,
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
GROUP BY grp
ORDER BY requests DESC`,
		grpExpr, chTable, where)

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
			errCount             int64
			tokens               map[string]int64
			avgMs                int64
			p50, p95, p99, maxMs int64
			ttft                 ttftCols
			costNanos, unpriced  int64
			firstSeen, lastSeen  time.Time
		)
		if err := rows.Scan(&groupVal, &requests, &errCount, &tokens, &avgMs, &p50, &p95, &p99, &maxMs,
			&ttft.count, &ttft.avg, &ttft.p50, &ttft.p95, &ttft.p99, &ttft.max,
			&costNanos, &unpriced,
			&firstSeen, &lastSeen); err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: scan summary: %w", err)
		}
		result.Rows = append(result.Rows, usage.SummaryRow{
			Group:      map[string]string{groupBy: groupVal},
			Requests:   requests,
			ErrorCount: errCount,
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

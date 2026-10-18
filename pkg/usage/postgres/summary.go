package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// Summary returns aggregated rows grouped by q.GroupBy. Token sums are
// computed by unnesting the jsonb map in SQL; latency percentiles use
// percentile_cont. Both queries share the same WHERE and are merged in Go.
func (s *Sink) Summary(ctx context.Context, q usage.SummaryQuery) (usage.SummaryResult, error) {
	groupBy := q.GroupBy
	if groupBy == "" {
		groupBy = "source"
	}
	if !usage.IsValidGroupBy(groupBy) {
		return usage.SummaryResult{}, fmt.Errorf("usage/postgres: invalid groupBy %q", groupBy)
	}

	where, args := buildWhere(q.EventQuery, true)
	grpExpr, args := groupExpr(groupBy, args)

	// Query 1: scalar aggregates + latency percentiles.
	scalarSQL := fmt.Sprintf(`
SELECT
    %s                                                       AS grp,
    count(*)::bigint                                         AS requests,
    count(*) FILTER (WHERE status >= 400)::bigint            AS error_count,
    avg(duration_ms)::bigint                                 AS avg_ms,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p50,
    percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p95,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p99,
    max(duration_ms)                                         AS max_ms,
    `+ttftSelectSQL+`,
    `+costSelectSQL+`,
    min(ts)                                                  AS first_seen,
    max(ts)                                                  AS last_seen
FROM %s%s
GROUP BY grp
ORDER BY requests DESC`,
		grpExpr, s.cfg.Table, where,
	)

	scalarRows, err := s.pool.Query(ctx, scalarSQL, args...)
	if err != nil {
		return usage.SummaryResult{}, fmt.Errorf("usage/postgres: summary scalar query: %w", err)
	}
	defer scalarRows.Close()

	type scalarRow struct {
		grp        string
		requests   int64
		errCount   int64
		avgMs      int64
		p50, p95   int64
		p99, maxMs int64
		ttft       ttftCols
		cost       int64
		unpriced   int64
		first      time.Time
		last       time.Time
	}
	var scalars []scalarRow
	for scalarRows.Next() {
		var r scalarRow
		if err := scalarRows.Scan(&r.grp, &r.requests, &r.errCount, &r.avgMs, &r.p50, &r.p95, &r.p99, &r.maxMs,
			&r.ttft.avg, &r.ttft.p50, &r.ttft.p95, &r.ttft.p99, &r.ttft.max,
			&r.cost, &r.unpriced,
			&r.first, &r.last); err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/postgres: scan summary scalar: %w", err)
		}
		scalars = append(scalars, r)
	}
	if err := scalarRows.Err(); err != nil {
		return usage.SummaryResult{}, err
	}

	groupIdx := make(map[string]int, len(scalars))
	rows := make([]usage.SummaryRow, len(scalars))
	for i, r := range scalars {
		groupIdx[r.grp] = i
		rows[i] = usage.SummaryRow{
			Group:      map[string]string{groupBy: r.grp},
			Requests:   r.requests,
			ErrorCount: r.errCount,
			Tokens:     map[string]int64{},
			DurationMs: usage.DurationStats{Avg: r.avgMs, P50: r.p50, P95: r.p95, P99: r.p99, Max: r.maxMs},
			TTFTMs:     r.ttft.stats(),
			FirstSeen:  r.first,
			LastSeen:   r.last,
			CostNanos:  r.cost,
			Unpriced:   r.unpriced,
		}
	}

	// Query 2: token sums per group via jsonb_each_text. The filtered rows are
	// wrapped in a subquery so WHERE precedes the CROSS JOIN LATERAL.
	if len(scalars) > 0 {
		tokenSQL := fmt.Sprintf(`
SELECT sub.grp, kv.key, sum(kv.value::bigint)::bigint
FROM (SELECT %s AS grp, tokens FROM %s%s) AS sub
CROSS JOIN LATERAL jsonb_each_text(sub.tokens) AS kv
GROUP BY sub.grp, kv.key`,
			grpExpr, s.cfg.Table, where,
		)
		tokenRows, err := s.pool.Query(ctx, tokenSQL, args...)
		if err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/postgres: summary token query: %w", err)
		}
		defer tokenRows.Close()

		for tokenRows.Next() {
			var grp, key string
			var val int64
			if err := tokenRows.Scan(&grp, &key, &val); err != nil {
				return usage.SummaryResult{}, fmt.Errorf("usage/postgres: scan token row: %w", err)
			}
			if idx, ok := groupIdx[grp]; ok {
				rows[idx].Tokens[key] = val
			}
		}
		if err := tokenRows.Err(); err != nil {
			return usage.SummaryResult{}, err
		}
	}

	var result usage.SummaryResult
	result.Rows = rows
	for _, r := range rows {
		if result.From.IsZero() || r.FirstSeen.Before(result.From) {
			result.From = r.FirstSeen
		}
		if r.LastSeen.After(result.To) {
			result.To = r.LastSeen
		}
	}
	return result, nil
}

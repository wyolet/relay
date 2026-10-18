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
	grouped := usage.Grouped(groupBy)
	selGroup, subGroup, groupClause := "", "", ungroupedHaving
	if grouped {
		var grpExpr string
		grpExpr, args = groupExpr(groupBy, args)
		selGroup, subGroup, groupClause = grpExpr+" AS grp,", "sub.grp, ", "GROUP BY grp"
	}

	// Query 1: scalar aggregates + latency percentiles.
	scalarSQL := fmt.Sprintf(`
SELECT
    %s
    count(*)::bigint                                         AS requests,
    `+errorSelectSQL+`,
    trunc(avg(duration_ms))::bigint                          AS avg_ms,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p50,
    percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p95,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p99,
    max(duration_ms)                                         AS max_ms,
    `+ttftSelectSQL+`,
    `+costSelectSQL+`,
    min(ts)                                                  AS first_seen,
    max(ts)                                                  AS last_seen
FROM %s%s
%s
ORDER BY requests DESC`,
		selGroup, s.cfg.Table, where, groupClause,
	)

	scalarRows, err := s.pool.Query(ctx, scalarSQL, args...)
	if err != nil {
		return usage.SummaryResult{}, fmt.Errorf("usage/postgres: summary scalar query: %w", err)
	}
	defer scalarRows.Close()

	type scalarRow struct {
		grp        string
		requests   int64
		errs       errorCols
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
		dest := []any{&r.requests, &r.errs.total, &r.errs.c4xx, &r.errs.c5xx, &r.errs.throttled,
			&r.avgMs, &r.p50, &r.p95, &r.p99, &r.maxMs,
			&r.ttft.avg, &r.ttft.p50, &r.ttft.p95, &r.ttft.p99, &r.ttft.max,
			&r.cost, &r.unpriced,
			&r.first, &r.last}
		if grouped {
			dest = append([]any{&r.grp}, dest...)
		}
		if err := scalarRows.Scan(dest...); err != nil {
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
			Group:      usage.GroupLabel(groupBy, r.grp),
			Requests:   r.requests,
			ErrorCount: r.errs.total,
			Errors4xx:  r.errs.c4xx,
			Errors5xx:  r.errs.c5xx,
			Throttled:  r.errs.throttled,
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
SELECT %skv.key, sum(kv.value::bigint)::bigint
FROM (SELECT %s tokens FROM %s%s) AS sub
CROSS JOIN LATERAL jsonb_each_text(sub.tokens) AS kv
GROUP BY %skv.key`,
			subGroup, selGroup, s.cfg.Table, where, subGroup,
		)
		tokenRows, err := s.pool.Query(ctx, tokenSQL, args...)
		if err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/postgres: summary token query: %w", err)
		}
		defer tokenRows.Close()

		for tokenRows.Next() {
			var grp, key string
			var val int64
			dest := []any{&key, &val}
			if grouped {
				dest = append([]any{&grp}, dest...)
			}
			if err := tokenRows.Scan(dest...); err != nil {
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

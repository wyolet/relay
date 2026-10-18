package clickhouse

import "github.com/wyolet/relay/pkg/usage"

// errorSelectSQL counts errors and their status-class split, matching usage.SummaryRow's invariant (any status >= 500 is 5xx; 429 is throttled).
const errorSelectSQL = `
    toInt64(countIf(status >= 400))                  AS error_count,
    toInt64(countIf(status >= 400 AND status < 500)) AS errors_4xx,
    toInt64(countIf(status >= 500))                  AS errors_5xx,
    toInt64(countIf(status = 429))                   AS throttled`

// ungroupedHaving replaces GROUP BY when rows aren't split by a dimension: an aggregate without GROUP BY returns one all-default row over an empty set, and this drops it so no events means no rows.
const ungroupedHaving = "HAVING count() > 0"

// ttftSelectSQL aggregates TTFT (upstream_response_start µs → ms) over rows
// that have upstream timing (sentinel -1 = none). The quantile/avg columns
// are NaN when no row qualifies — scanned as Float64 and gated on ttft_count
// in ttftCols.stats rather than guarded in SQL.
const ttftSelectSQL = `
    toInt64(countIf(upstream_response_start >= 0))                                  AS ttft_count,
    avgIf(upstream_response_start / 1000, upstream_response_start >= 0)             AS ttft_avg,
    quantileIf(0.5)(upstream_response_start / 1000, upstream_response_start >= 0)   AS ttft_p50,
    quantileIf(0.95)(upstream_response_start / 1000, upstream_response_start >= 0)  AS ttft_p95,
    quantileIf(0.99)(upstream_response_start / 1000, upstream_response_start >= 0)  AS ttft_p99,
    toInt64(maxIf(intDiv(upstream_response_start, 1000), upstream_response_start >= 0)) AS ttft_max`

// costSelectSQL sums emit-time cost over priced rows only (sentinel -1 =
// unpriced) and counts the unpriced ones — a cost total must say how
// complete it is rather than fold unpriced rows in as silent zeros.
// The sum's alias must NOT be `cost_nanos`: ClickHouse resolves identifiers
// to SELECT aliases before columns, so an alias shadowing the column would
// substitute the sumIf into countIf's argument — "aggregate function found
// inside another aggregate function" (code 184). Scans are positional, so
// the alias name is otherwise free.
const costSelectSQL = `
    toInt64(sumIf(cost_nanos, cost_nanos >= 0)) AS cost_nanos_sum,
    toInt64(countIf(cost_nanos < 0))            AS unpriced`

// errorCols receives the errorSelectSQL columns for one result row.
type errorCols struct {
	total, c4xx, c5xx, throttled int64
}

// ttftCols receives the ttftSelectSQL columns for one result row.
type ttftCols struct {
	count              int64
	avg, p50, p95, p99 float64
	max                int64
}

func (t ttftCols) stats() *usage.DurationStats {
	if t.count == 0 {
		return nil
	}
	return &usage.DurationStats{
		Avg: int64(t.avg),
		P50: int64(t.p50),
		P95: int64(t.p95),
		P99: int64(t.p99),
		Max: t.max,
	}
}

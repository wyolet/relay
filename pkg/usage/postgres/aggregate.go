package postgres

import (
	"fmt"

	"github.com/wyolet/relay/pkg/usage"
)

// ttftSelectSQL aggregates TTFT (upstream_response_start µs → ms) over rows
// that have upstream timing (column is NULL when the request never reached
// upstream — aggregates skip NULLs; FILTER keeps percentile_cont consistent).
// All columns are NULL when no row in the group qualifies.
const ttftSelectSQL = `
    (avg(upstream_response_start / 1000))::bigint AS ttft_avg,
    (percentile_cont(0.5) WITHIN GROUP (ORDER BY upstream_response_start / 1000) FILTER (WHERE upstream_response_start IS NOT NULL))::bigint  AS ttft_p50,
    (percentile_cont(0.95) WITHIN GROUP (ORDER BY upstream_response_start / 1000) FILTER (WHERE upstream_response_start IS NOT NULL))::bigint AS ttft_p95,
    (percentile_cont(0.99) WITHIN GROUP (ORDER BY upstream_response_start / 1000) FILTER (WHERE upstream_response_start IS NOT NULL))::bigint AS ttft_p99,
    max(upstream_response_start / 1000)           AS ttft_max`

// costSelectSQL sums emit-time cost over priced rows only (cost_nanos NULL
// = unpriced) and counts the unpriced ones — a cost total must say how
// complete it is rather than fold unpriced rows in as silent zeros.
const costSelectSQL = `
    COALESCE(sum(cost_nanos), 0)::bigint                  AS cost_nanos,
    count(*) FILTER (WHERE cost_nanos IS NULL)::bigint    AS unpriced`

// ttftCols receives the ttftSelectSQL columns; pointers carry SQL NULL for
// the no-samples case.
type ttftCols struct {
	avg, p50, p95, p99, max *int64
}

func (t ttftCols) stats() *usage.DurationStats {
	if t.avg == nil {
		return nil
	}
	deref := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return &usage.DurationStats{
		Avg: *t.avg,
		P50: deref(t.p50),
		P95: deref(t.p95),
		P99: deref(t.p99),
		Max: deref(t.max),
	}
}

// groupExpr returns the SQL grouping expression for groupBy. Static
// dimensions are column names; the dynamic "tags.<key>" / "extras.<key>"
// dimensions bind the key as a positional arg (never spliced into SQL text)
// and group on the map value, missing key folded to ”.
func groupExpr(groupBy string, args []any) (string, []any) {
	if column, key, ok := usage.MapGroupKey(groupBy); ok {
		args = append(args, key)
		return fmt.Sprintf("COALESCE(%s->>$%d, '')", column, len(args)), args
	}
	return groupBy, args
}

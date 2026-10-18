package clickhouse

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/pkg/usage"
)

// Facet counts q.Dim's values in one GROUP BY, labelling id dimensions with their latest non-empty event-time slug.
func (s *Sink) Facet(ctx context.Context, q usage.FacetQuery) ([]usage.FacetValue, error) {
	if !usage.IsValidFacetDim(q.Dim) {
		return nil, fmt.Errorf("usage/clickhouse: invalid facet dim %q", q.Dim)
	}
	where, args := buildWhere(q.EventQuery, true)
	dimExpr := q.Dim // column name matches the dimension in this schema
	if column, key, ok := usage.MapGroupKey(q.Dim); ok {
		// The key precedes the WHERE args: placeholders bind in text order.
		dimExpr = column + "[?]"
		args = append([]any{key}, args...)
	}
	labelExpr := "''"
	if expr, _, ok := latestLabelSQL(q.Dim); ok {
		labelExpr = expr
	}

	sql := fmt.Sprintf(`
SELECT %s AS v, %s AS lbl, toInt64(count()) AS requests
FROM %s%s
GROUP BY v
ORDER BY requests DESC, v
LIMIT %d`, dimExpr, labelExpr, chTable, where, usage.FacetLimit(q.Limit))

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("usage/clickhouse: facet query: %w", err)
	}
	defer rows.Close()

	var out []usage.FacetValue
	for rows.Next() {
		var fv usage.FacetValue
		if err := rows.Scan(&fv.Value, &fv.Label, &fv.Requests); err != nil {
			return nil, fmt.Errorf("usage/clickhouse: scan facet: %w", err)
		}
		out = append(out, fv)
	}
	return out, rows.Err()
}

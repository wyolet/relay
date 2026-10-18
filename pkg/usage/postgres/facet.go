package postgres

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/pkg/usage"
)

// Facet counts q.Dim's values in one GROUP BY, labelling id dimensions with their latest non-empty event-time slug. The value tiebreak sorts under the C collation so it orders bytewise like the other backends.
func (s *Sink) Facet(ctx context.Context, q usage.FacetQuery) ([]usage.FacetValue, error) {
	if !usage.IsValidFacetDim(q.Dim) {
		return nil, fmt.Errorf("usage/postgres: invalid facet dim %q", q.Dim)
	}
	where, args := buildWhere(q.EventQuery, true)
	var dimExpr string
	dimExpr, args = groupExpr(q.Dim, args)
	labelExpr := "''"
	if expr, _, ok := latestLabelSQL(q.Dim); ok {
		labelExpr = expr
	}

	sql := fmt.Sprintf(`
SELECT (%s) COLLATE "C" AS v, %s AS lbl, count(*)::bigint AS requests
FROM %s%s
GROUP BY v
ORDER BY requests DESC, v
LIMIT %d`, dimExpr, labelExpr, s.cfg.Table, where, usage.FacetLimit(q.Limit))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("usage/postgres: facet query: %w", err)
	}
	defer rows.Close()

	var out []usage.FacetValue
	for rows.Next() {
		var fv usage.FacetValue
		if err := rows.Scan(&fv.Value, &fv.Label, &fv.Requests); err != nil {
			return nil, fmt.Errorf("usage/postgres: scan facet: %w", err)
		}
		out = append(out, fv)
	}
	return out, rows.Err()
}

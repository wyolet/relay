package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// Count returns how many events Events would match, up to limit. The inner LIMIT lets Postgres stop scanning once the cap is reached.
func (s *Sink) Count(ctx context.Context, q usage.EventQuery, limit int) (int64, error) {
	q.CursorTS, q.CursorID = time.Time{}, ""
	where, args := buildWhere(q, false)
	sql := fmt.Sprintf("SELECT count(*) FROM %s%s", s.cfg.Table, where)
	if limit > 0 {
		sql = fmt.Sprintf("SELECT count(*) FROM (SELECT 1 FROM %s%s LIMIT %d) AS sub", s.cfg.Table, where, limit)
	}
	var n int64
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("usage/postgres: count query: %w", err)
	}
	return n, nil
}

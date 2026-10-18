package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

// Count returns how many events Events would match, up to limit. The inner LIMIT lets ClickHouse stop reading once the cap is reached.
func (s *Sink) Count(ctx context.Context, q usage.EventQuery, limit int) (int64, error) {
	q.CursorTS, q.CursorID = time.Time{}, ""
	where, args := buildWhere(q, false)
	sql := "SELECT toInt64(count()) FROM " + chTable + where
	if limit > 0 {
		sql = fmt.Sprintf("SELECT toInt64(count()) FROM (SELECT 1 FROM %s%s LIMIT %d)", chTable, where, limit)
	}
	var n int64
	if err := s.conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("usage/clickhouse: count query: %w", err)
	}
	return n, nil
}

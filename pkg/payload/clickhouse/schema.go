package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const chTable = "payload_logs"

// Body-only: the request/response bytes keyed by request_id, nothing else.
// All per-request metadata lives on the log event (usage_events) and is
// joined by request_id at the API layer. Bodies are large and highly
// compressible — ZSTD(3) trades a little CPU for a much better ratio. The
// bloom_filter skip index on request_id makes Get (a point lookup with no
// time bound, so no partition pruning) skip most granules.
var createTableSQL = `CREATE TABLE IF NOT EXISTS payload_logs (
    request_id          String                 CODEC(ZSTD),
    ts                  DateTime64(9, 'UTC')   CODEC(DoubleDelta),
    request_body        String                 CODEC(ZSTD(3)),
    response_body       String                 CODEC(ZSTD(3)),
    request_truncated   UInt8,
    response_truncated  UInt8,
    INDEX idx_request_id request_id TYPE bloom_filter GRANULARITY 4
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (ts, request_id)
TTL toDateTime(ts) + INTERVAL %d DAY`

// expectedColumns is the column set insertBatch writes. Used by ensureSchema
// to detect a pre-existing incompatible table.
var expectedColumns = []string{
	"request_id", "ts",
	"request_body", "response_body", "request_truncated", "response_truncated",
}

// ensureSchema creates the table if absent, then verifies its columns match
// what insertBatch writes. CREATE TABLE IF NOT EXISTS silently no-ops against
// a pre-existing (possibly older) table, which would make every insert fail
// forever — so fail fast with an actionable error instead of auto-dropping.
func ensureSchema(ctx context.Context, conn clickhouse.Conn, retentionDays int) error {
	if err := conn.Exec(ctx, fmt.Sprintf(createTableSQL, retentionDays)); err != nil {
		return fmt.Errorf("payload/clickhouse: create table: %w", err)
	}

	rows, err := conn.Query(ctx,
		"SELECT name FROM system.columns WHERE database = currentDatabase() AND table = ?", chTable)
	if err != nil {
		return fmt.Errorf("payload/clickhouse: describe %s: %w", chTable, err)
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("payload/clickhouse: scan column: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var missing []string
	for _, c := range expectedColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"payload/clickhouse: table %q exists with an incompatible schema (missing columns: %s) — drop or rename it (or point at a fresh database) so relay can create the current schema",
			chTable, strings.Join(missing, ", "))
	}
	return nil
}

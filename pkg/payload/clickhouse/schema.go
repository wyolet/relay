package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const chTable = "payload_logs"

// The request/response bytes keyed by request_id, plus the owner columns
// the API layer matches against the joined log event (usage_events). Bodies
// are large and highly
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
    project_id          String                 CODEC(ZSTD),
    principal_id        String                 CODEC(ZSTD),
    relay_key_hash      String                 CODEC(ZSTD),
    INDEX idx_request_id request_id TYPE bloom_filter GRANULARITY 4
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (ts, request_id)
TTL toDateTime(ts) + INTERVAL %d DAY`

const addOwnerColumnsSQL = `ALTER TABLE payload_logs
    ADD COLUMN IF NOT EXISTS project_id     String CODEC(ZSTD),
    ADD COLUMN IF NOT EXISTS principal_id   String CODEC(ZSTD),
    ADD COLUMN IF NOT EXISTS relay_key_hash String CODEC(ZSTD)`

// insertColumns is the column list insertBatch writes, in Append order.
// Used by ensureSchema to detect a pre-existing incompatible table.
var insertColumns = []string{
	"request_id", "ts",
	"request_body", "response_body", "request_truncated", "response_truncated",
	"project_id", "principal_id", "relay_key_hash",
}

// InsertSQL is the INSERT statement matching insertColumns, shared with
// tools that write the table directly.
var InsertSQL = "INSERT INTO " + chTable + " (" + strings.Join(insertColumns, ", ") + ")"

// ensureSchema creates the table if absent, adds the owner columns to an
// older table, then verifies its columns match what insertBatch writes.
// Anything else missing fails fast with an actionable error instead of
// auto-dropping.
func ensureSchema(ctx context.Context, conn clickhouse.Conn, retentionDays int) error {
	if err := conn.Exec(ctx, fmt.Sprintf(createTableSQL, retentionDays)); err != nil {
		return fmt.Errorf("payload/clickhouse: create table: %w", err)
	}
	if err := conn.Exec(ctx, addOwnerColumnsSQL); err != nil {
		return fmt.Errorf("payload/clickhouse: add owner columns: %w", err)
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
	for _, c := range insertColumns {
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

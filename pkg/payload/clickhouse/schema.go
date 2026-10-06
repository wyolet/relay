package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/wyolet/relay/pkg/chttl"
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
ORDER BY (ts, request_id)`

const addOwnerColumnsSQL = `ALTER TABLE payload_logs
    ADD COLUMN IF NOT EXISTS project_id     String CODEC(ZSTD),
    ADD COLUMN IF NOT EXISTS principal_id   String CODEC(ZSTD),
    ADD COLUMN IF NOT EXISTS relay_key_hash String CODEC(ZSTD)`

const (
	piecesTable   = "payload_pieces"
	requestsTable = "payload_requests"
)

// Each distinct request-body piece once per project, shared by every request
// of that project that contains it; projects never share a row, so erasing
// one cannot touch another's content. ReplacingMergeTree keeps the newest
// last_seen per (project_id, hash); the sink re-inserts a piece in use at
// least daily, so the last_seen TTL only drops pieces no live request row
// references. The server stamps last_seen, so an erase can compare it with
// its own start time whatever the writing pod's clock says.
var createPiecesSQL = `CREATE TABLE IF NOT EXISTS payload_pieces (
    project_id String         CODEC(ZSTD),
    hash       FixedString(16),
    body       String         CODEC(ZSTD(3)),
    last_seen  DateTime64(3)  DEFAULT now64(3) CODEC(DoubleDelta)
) ENGINE = ReplacingMergeTree(last_seen)
ORDER BY (project_id, hash)`

// piecesLayout and requestsLayout are the columns an earlier layout of each
// table lacked or typed differently.
var (
	piecesLayout   = map[string]string{"project_id": "String", "last_seen": "DateTime64(3)"}
	requestsLayout = map[string]string{"request_sha256": "FixedString(32)"}
)

// One row per request: the body's skeleton plus, per split field, its name,
// kind, skeleton offset and how many entries of hashes it owns.
// request_sha256 is the digest of the body as received; a read that does
// not rebuild to it fails.
var createRequestsSQL = `CREATE TABLE IF NOT EXISTS payload_requests (
    request_id          String                CODEC(ZSTD),
    ts                  DateTime64(9, 'UTC')  CODEC(DoubleDelta),
    project_id          String                CODEC(ZSTD),
    principal_id        String                CODEC(ZSTD),
    relay_key_hash      String                CODEC(ZSTD),
    skeleton            String                CODEC(ZSTD(3)),
    field_names         Array(String),
    field_is_array      Array(UInt8),
    field_offsets       Array(UInt32),
    field_counts        Array(UInt32),
    hashes              Array(FixedString(16)),
    request_sha256      FixedString(32),
    response_body       String                CODEC(ZSTD(3)),
    request_truncated   UInt8,
    response_truncated  UInt8,
    INDEX idx_request_id request_id TYPE bloom_filter GRANULARITY 4
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (ts, request_id)`

var insertPiecesSQL = "INSERT INTO " + piecesTable + " (project_id, hash, body)"

var insertRequestsSQL = "INSERT INTO " + requestsTable + ` (request_id, ts, project_id, principal_id, relay_key_hash,
    skeleton, field_names, field_is_array, field_offsets, field_counts, hashes, request_sha256,
    response_body, request_truncated, response_truncated)`

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

// ensureSchema creates the tables if absent, adds the owner columns to an
// older payload_logs, recreates empty dedup tables of an earlier layout,
// then verifies payload_logs' columns match what insertBatch writes.
// Anything else missing fails fast with an actionable error instead of
// auto-dropping. All three tables exist in either write mode, so a reader
// finds rows whichever mode wrote them.
func ensureSchema(ctx context.Context, conn clickhouse.Conn, retentionDays int) error {
	if err := conn.Exec(ctx, createTableSQL); err != nil {
		return fmt.Errorf("payload/clickhouse: create table: %w", err)
	}
	if err := conn.Exec(ctx, createPiecesSQL); err != nil {
		return fmt.Errorf("payload/clickhouse: create %s: %w", piecesTable, err)
	}
	if err := conn.Exec(ctx, createRequestsSQL); err != nil {
		return fmt.Errorf("payload/clickhouse: create %s: %w", requestsTable, err)
	}
	if err := conn.Exec(ctx, addOwnerColumnsSQL); err != nil {
		return fmt.Errorf("payload/clickhouse: add owner columns: %w", err)
	}
	if err := replaceOutdated(ctx, conn, piecesTable, piecesLayout, createPiecesSQL); err != nil {
		return err
	}
	if err := replaceOutdated(ctx, conn, requestsTable, requestsLayout, createRequestsSQL); err != nil {
		return err
	}

	have, err := tableColumns(ctx, conn, chTable)
	if err != nil {
		return err
	}
	var missing []string
	for _, c := range insertColumns {
		if _, ok := have[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"payload/clickhouse: table %q exists with an incompatible schema (missing columns: %s) — drop or rename it (or point at a fresh database) so relay can create the current schema",
			chTable, strings.Join(missing, ", "))
	}
	if err := chttl.Apply(ctx, conn, chTable, "ts", retentionDays); err != nil {
		return err
	}
	if err := chttl.Apply(ctx, conn, requestsTable, "ts", retentionDays); err != nil {
		return err
	}
	return chttl.Apply(ctx, conn, piecesTable, "last_seen", piecesRetentionDays(retentionDays))
}

// replaceOutdated recreates table in the current layout when any column of
// want is missing or has another type, as in a table from an earlier
// layout. Only an empty table is dropped; one holding rows fails instead,
// so no captured body is lost.
func replaceOutdated(ctx context.Context, conn clickhouse.Conn, table string, want map[string]string, createSQL string) error {
	have, err := tableColumns(ctx, conn, table)
	if err != nil {
		return err
	}
	var outdated []string
	for col, typ := range want {
		if have[col] != typ {
			outdated = append(outdated, col+" "+typ)
		}
	}
	if len(outdated) == 0 {
		return nil
	}
	sort.Strings(outdated)
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+table).Scan(&n); err != nil {
		return fmt.Errorf("payload/clickhouse: count %s: %w", table, err)
	}
	if n > 0 {
		return fmt.Errorf(
			"payload/clickhouse: table %q holds %d rows in an earlier layout (needs columns: %s) — drop or rename it (or point at a fresh database) so relay can create the current schema",
			table, n, strings.Join(outdated, ", "))
	}
	if err := conn.Exec(ctx, "DROP TABLE "+table+" SYNC"); err != nil {
		return fmt.Errorf("payload/clickhouse: drop empty %s: %w", table, err)
	}
	if err := conn.Exec(ctx, createSQL); err != nil {
		return fmt.Errorf("payload/clickhouse: recreate %s: %w", table, err)
	}
	return nil
}

// tableColumns maps each column of table to its type.
func tableColumns(ctx context.Context, conn clickhouse.Conn, table string) (map[string]string, error) {
	rows, err := conn.Query(ctx,
		"SELECT name, type FROM system.columns WHERE database = currentDatabase() AND table = ?", table)
	if err != nil {
		return nil, fmt.Errorf("payload/clickhouse: describe %s: %w", table, err)
	}
	defer rows.Close()
	have := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, fmt.Errorf("payload/clickhouse: scan column: %w", err)
		}
		have[name] = typ
	}
	return have, rows.Err()
}

// piecesRetentionDays outlives request rows by a day: a piece's last_seen is
// refreshed at most once per UTC day, so it can trail the newest request
// that references it by up to a day.
func piecesRetentionDays(requestDays int) int {
	if requestDays == 0 {
		return 0
	}
	return requestDays + 1
}

package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/wyolet/relay/pkg/payload"
)

// Compile-time interface assertions.
var _ payload.Sink = (*Sink)(nil)
var _ payload.Reader = (*Sink)(nil)
var _ payload.Closer = (*Sink)(nil)

// Config holds all tunables for the ClickHouse payload sink. It mirrors the
// usage CH sink's config; the WAL knobs are shared, plus a byte-based
// rotation threshold since payload bodies are MB-scale (a line-count cap
// alone would let a segment grow to gigabytes before rotating).
type Config struct {
	// DSN is the ClickHouse connection string (clickhouse://host:port/db).
	DSN string

	// RetentionDays controls the MergeTree TTL. Default 30 — payload bodies
	// are bulky and short-lived debug/audit artifacts, so a shorter default
	// than the usage sink's 90.
	RetentionDays int

	// WALDir is the directory for WAL segment files.
	WALDir string

	// MaxLines is the number of records per WAL segment before rotation.
	// Default 2000 (lower than usage's 10k — records are large).
	MaxLines int

	// MaxBytes rotates the active segment once it exceeds this size,
	// whichever comes first with MaxLines. Default 64 MiB.
	MaxBytes int

	// FlushInterval is how often the background goroutine rotates and
	// flushes pending segments. Default 10s.
	FlushInterval time.Duration

	// MaxSegments caps how many pending segment files may accumulate on
	// disk. When exceeded, the oldest are dropped and counted in Dropped().
	// Default 256.
	MaxSegments int
}

func (c *Config) applyDefaults() {
	if c.RetentionDays <= 0 {
		c.RetentionDays = 30
	}
	if c.MaxLines <= 0 {
		c.MaxLines = 2000
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 64 << 20
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 10 * time.Second
	}
	if c.MaxSegments <= 0 {
		c.MaxSegments = 256
	}
}

const chTable = "payload_logs"

// Bodies are large and highly compressible — ZSTD(3) trades a little CPU for
// a much better ratio than the default level. The bloom_filter skip index on
// request_id makes Get (a point lookup with no time bound, so no partition
// pruning) skip most granules instead of scanning the table.
var createTableSQL = `CREATE TABLE IF NOT EXISTS payload_logs (
    request_id          String                 CODEC(ZSTD),
    ts                  DateTime64(9, 'UTC')   CODEC(DoubleDelta),
    source              LowCardinality(String),
    status              UInt16,
    streamed            UInt8,
    relay_key_hash      String,
    policy_id           String,
    model_id            String,
    host_id             String,
    error_kind          LowCardinality(String),
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
	"request_id", "ts", "source", "status", "streamed",
	"relay_key_hash", "policy_id", "model_id", "host_id", "error_kind",
	"request_body", "response_body", "request_truncated", "response_truncated",
}

// listColumns are the metadata columns List selects — deliberately excludes
// the body columns so the table view never reads (or ships) captured bodies.
// This is the column-projection win ClickHouse gives over the flat file/S3
// readers, which must read whole rows to filter.
const listColumns = "request_id, ts, source, status, streamed, relay_key_hash, policy_id, model_id, host_id, error_kind, request_truncated, response_truncated"

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

// Reader is the read-only half: a ClickHouse connection serving List/Get with
// no WAL. The /payloads endpoints use this so the read path carries none of
// the write-side machinery (segment queue, recovery). Implements
// payload.Reader + payload.Closer.
type Reader struct {
	conn clickhouse.Conn
	log  *slog.Logger
}

var _ payload.Reader = (*Reader)(nil)
var _ payload.Closer = (*Reader)(nil)

// Sink is the write half: Reader + a WAL-backed insert path. Implements
// payload.Sink (and, via the embedded Reader, payload.Reader) + payload.Closer.
type Sink struct {
	*Reader
	wal *segmentQueue
}

// openConn parses the DSN, opens a pooled connection, pings it, and ensures
// the schema. Shared by New and NewReader.
func openConn(cfg Config) (clickhouse.Conn, error) {
	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("payload/clickhouse: parse DSN: %w", err)
	}
	opts.MaxOpenConns = 4
	opts.MaxIdleConns = 2
	opts.ConnMaxLifetime = time.Hour
	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	opts.Settings["async_insert"] = 1
	opts.Settings["wait_for_async_insert"] = 1

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("payload/clickhouse: open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("payload/clickhouse: ping: %w", err)
	}
	if err := ensureSchema(pingCtx, conn, cfg.RetentionDays); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// NewReader opens a read-only connection (no WAL). ensureSchema runs so the
// Logs page works against an empty table before the first write.
func NewReader(cfg Config) (*Reader, error) {
	cfg.applyDefaults()
	conn, err := openConn(cfg)
	if err != nil {
		return nil, err
	}
	return &Reader{conn: conn, log: slog.Default()}, nil
}

// Close closes the underlying connection.
func (r *Reader) Close() error { return r.conn.Close() }

// New opens a ClickHouse connection, ensures the schema, constructs the WAL
// segment queue, and drains any segments left from a previous run.
func New(cfg Config) (*Sink, error) {
	cfg.applyDefaults()

	conn, err := openConn(cfg)
	if err != nil {
		return nil, err
	}

	s := &Sink{Reader: &Reader{conn: conn, log: slog.Default()}}

	wal, err := newSegmentQueue(cfg.WALDir, cfg.MaxLines, cfg.MaxBytes,
		cfg.FlushInterval, cfg.MaxSegments, s.log, s.insertBatch)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s.wal = wal
	s.wal.Recover()

	return s, nil
}

// Write appends r to the WAL. Returns as soon as the record is durable on the
// local filesystem; CH delivery is asynchronous.
func (s *Sink) Write(r payload.Record) error {
	return s.wal.Write(r)
}

// Close flushes the WAL and closes the ClickHouse connection.
func (s *Sink) Close() error {
	if err := s.wal.Close(); err != nil {
		s.log.Warn("payload/clickhouse: wal close", "err", err)
	}
	return s.Reader.Close()
}

// Dropped returns the number of records dropped due to the WAL maxSegments cap.
func (s *Sink) Dropped() uint64 { return s.wal.Dropped() }

// insertBatch is the flushFn injected into the segmentQueue.
func (s *Sink) insertBatch(records []payload.Record) error {
	if len(records) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO "+chTable)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}

	for _, r := range records {
		if err := batch.Append(
			r.RequestID,
			r.Timestamp,
			r.Source,
			uint16(r.Status),
			b2u8(r.Streamed),
			r.RelayKeyHash,
			r.PolicyID,
			r.ModelID,
			r.HostID,
			r.ErrorKind,
			string(r.RequestBody),
			string(r.ResponseBody),
			b2u8(r.RequestTruncated),
			b2u8(r.ResponseTruncated),
		); err != nil {
			return fmt.Errorf("append row: %w", err)
		}
	}
	return batch.Send()
}

// List returns matching records newest-first with bodies stripped — the body
// columns are never selected, so the query reads only the metadata columns
// off disk.
func (s *Reader) List(ctx context.Context, q payload.Query) ([]payload.Record, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = payload.DefaultLimit
	}
	if limit > payload.MaxLimit {
		limit = payload.MaxLimit
	}

	where, args := buildWhere(q)
	sql := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY ts DESC, request_id DESC LIMIT %d",
		listColumns, chTable, where, limit)

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("payload/clickhouse: list query: %w", err)
	}
	defer rows.Close()

	var out []payload.Record
	for rows.Next() {
		var (
			r         payload.Record
			status    uint16
			streamed  uint8
			reqTrunc  uint8
			respTrunc uint8
		)
		if err := rows.Scan(
			&r.RequestID, &r.Timestamp, &r.Source, &status, &streamed,
			&r.RelayKeyHash, &r.PolicyID, &r.ModelID, &r.HostID, &r.ErrorKind,
			&reqTrunc, &respTrunc,
		); err != nil {
			return nil, fmt.Errorf("payload/clickhouse: scan list row: %w", err)
		}
		r.Status = int(status)
		r.Streamed = streamed == 1
		r.RequestTruncated = reqTrunc == 1
		r.ResponseTruncated = respTrunc == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns the full record (bodies included) for one request id. The
// newest row wins if an id was somehow reused. Returns payload.ErrNotFound
// when absent.
func (s *Reader) Get(ctx context.Context, requestID string) (payload.Record, error) {
	sql := fmt.Sprintf(
		"SELECT request_id, ts, source, status, streamed, relay_key_hash, policy_id, model_id, host_id, error_kind, request_body, response_body, request_truncated, response_truncated FROM %s WHERE request_id = ? ORDER BY ts DESC LIMIT 1",
		chTable)

	rows, err := s.conn.Query(ctx, sql, requestID)
	if err != nil {
		return payload.Record{}, fmt.Errorf("payload/clickhouse: get query: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return payload.Record{}, err
		}
		return payload.Record{}, payload.ErrNotFound
	}

	var (
		r                   payload.Record
		status              uint16
		streamed            uint8
		reqTrunc, respTrunc uint8
		reqBody, respBody   string
	)
	if err := rows.Scan(
		&r.RequestID, &r.Timestamp, &r.Source, &status, &streamed,
		&r.RelayKeyHash, &r.PolicyID, &r.ModelID, &r.HostID, &r.ErrorKind,
		&reqBody, &respBody, &reqTrunc, &respTrunc,
	); err != nil {
		return payload.Record{}, fmt.Errorf("payload/clickhouse: scan get row: %w", err)
	}
	r.Status = int(status)
	r.Streamed = streamed == 1
	r.RequestTruncated = reqTrunc == 1
	r.ResponseTruncated = respTrunc == 1
	if reqBody != "" {
		r.RequestBody = []byte(reqBody)
	}
	if respBody != "" {
		r.ResponseBody = []byte(respBody)
	}
	return r, rows.Err()
}

// buildWhere generates the WHERE clause + positional args for a payload.Query.
// Mirrors the usage CH buildWhere, narrowed to the dimensions a Record carries.
func buildWhere(q payload.Query) (string, []any) {
	var clauses []string
	var args []any

	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		clauses = append(clauses, col+" IN ("+strings.Join(ph, ",")+")")
	}

	if !q.From.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, q.From)
	} else if q.Since > 0 {
		clauses = append(clauses, fmt.Sprintf("ts >= now() - INTERVAL %d SECOND", int64(q.Since.Seconds())))
	}
	if !q.To.IsZero() {
		clauses = append(clauses, "ts <= ?")
		args = append(args, q.To)
	}
	if !q.CursorTS.IsZero() {
		clauses = append(clauses, "(ts, request_id) < (?, ?)")
		args = append(args, q.CursorTS, q.CursorID)
	}
	in("relay_key_hash", q.RelayKeyHash)
	in("policy_id", q.PolicyID)
	in("model_id", q.ModelID)
	in("host_id", q.HostID)
	in("source", q.Source)
	in("error_kind", q.ErrorKind)
	if q.StatusMin > 0 {
		clauses = append(clauses, "status >= ?")
		args = append(args, uint16(q.StatusMin))
	}
	if q.StatusMax > 0 {
		clauses = append(clauses, "status <= ?")
		args = append(args, uint16(q.StatusMax))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/wyolet/relay/pkg/usage"
)

// Compile-time interface assertions.
var _ usage.Sink = (*Sink)(nil)
var _ usage.Reader = (*Sink)(nil)
var _ usage.Closer = (*Sink)(nil)

// Config holds all tunables for the ClickHouse sink.
type Config struct {
	// DSN is the ClickHouse connection string (clickhouse://host:port/db).
	DSN string

	// RetentionDays controls the MergeTree TTL. Default 90.
	RetentionDays int

	// WALDir is the directory for WAL segment files.
	WALDir string

	// MaxLines is the number of events per WAL segment before rotation.
	// Default 10000.
	MaxLines int

	// FlushInterval is how often the background goroutine rotates and
	// flushes pending segments. Default 10s.
	FlushInterval time.Duration

	// MaxSegments caps how many pending segment files may accumulate on
	// disk. When exceeded, the oldest segments are dropped and counted in
	// Dropped(). Default 1024.
	MaxSegments int
}

func (c *Config) applyDefaults() {
	if c.RetentionDays <= 0 {
		c.RetentionDays = 90
	}
	if c.MaxLines <= 0 {
		c.MaxLines = 10_000
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 10 * time.Second
	}
	if c.MaxSegments <= 0 {
		c.MaxSegments = 1024
	}
}

const chTable = "usage_events"

var createTableSQL = `CREATE TABLE IF NOT EXISTS usage_events (
    request_id               String                      CODEC(ZSTD),
    source                   LowCardinality(String),
    ts                       DateTime64(9, 'UTC')        CODEC(DoubleDelta),
    status                   UInt16,
    duration_ms              Int64,
    streamed                 UInt8,
    finish_reason            LowCardinality(String),
    attempts                 UInt16,
    error_kind               LowCardinality(String),
    error_message            String                      CODEC(ZSTD),
    upstream_start           Int64,
    upstream_response_start  Int64,
    upstream_response_end    Int64,
    relay_key_hash           String,
    policy_id                String,
    model_id                 String,
    requested_model          LowCardinality(String),
    host_id                  String,
    host_key_id              String,
    tokens                   Map(LowCardinality(String), Int64) CODEC(ZSTD(1)),
    extras                   Map(LowCardinality(String), String)
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (ts, model_id, policy_id)
TTL toDateTime(ts) + INTERVAL %d DAY`

// Sink is the ClickHouse-backed implementation of usage.Sink, usage.Reader,
// and usage.Closer.
type Sink struct {
	conn clickhouse.Conn
	wal  *segmentQueue
	log  *slog.Logger
}

// New opens a ClickHouse connection, ensures the schema exists, constructs
// the WAL segment queue, and drains any segments left from a previous run.
func New(cfg Config) (*Sink, error) {
	cfg.applyDefaults()

	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("usage/clickhouse: parse DSN: %w", err)
	}
	opts.MaxOpenConns = 4
	opts.MaxIdleConns = 2
	opts.ConnMaxLifetime = time.Hour

	// Server-side async insert: CH coalesces concurrent INSERTs into larger
	// MergeTree parts, avoiding one-part-per-INSERT merge amplification.
	// wait_for_async_insert=1 preserves back-pressure / error visibility.
	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	opts.Settings["async_insert"] = 1
	opts.Settings["wait_for_async_insert"] = 1

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("usage/clickhouse: open: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("usage/clickhouse: ping: %w", err)
	}

	ddl := fmt.Sprintf(createTableSQL, cfg.RetentionDays)
	if err := conn.Exec(pingCtx, ddl); err != nil {
		conn.Close()
		return nil, fmt.Errorf("usage/clickhouse: ensure schema: %w", err)
	}

	s := &Sink{
		conn: conn,
		log:  slog.Default(),
	}

	wal, err := newSegmentQueue(
		cfg.WALDir,
		cfg.MaxLines,
		cfg.FlushInterval,
		cfg.MaxSegments,
		s.log,
		s.insertBatch,
	)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s.wal = wal
	s.wal.Recover()

	return s, nil
}

// Write appends ev to the WAL. The call returns as soon as the event is
// durable on the local filesystem; CH delivery is asynchronous.
func (s *Sink) Write(ev usage.Event) error {
	return s.wal.Write(ev)
}

// Close flushes the WAL and closes the ClickHouse connection.
func (s *Sink) Close() error {
	if err := s.wal.Close(); err != nil {
		s.log.Warn("usage/clickhouse: wal close", "err", err)
	}
	return s.conn.Close()
}

// Dropped returns the number of events dropped due to WAL maxSegments cap.
func (s *Sink) Dropped() uint64 {
	return s.wal.Dropped()
}

// insertBatch is the flushFn injected into the segmentQueue. It performs a
// single CH batch insert for all events in the slice.
func (s *Sink) insertBatch(events []usage.Event) error {
	if len(events) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO "+chTable)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}

	for _, ev := range events {
		// Sentinel -1 means "no upstream timing available" in Int64 columns.
		var upStart, upRespStart, upRespEnd int64 = -1, -1, -1
		if ev.Upstream != nil {
			upStart = ev.Upstream.Start
			upRespStart = ev.Upstream.ResponseStart
			upRespEnd = ev.Upstream.ResponseEnd
		}

		tokens := map[string]int64(ev.Tokens)
		if tokens == nil {
			tokens = map[string]int64{}
		}
		extras := ev.Extras
		if extras == nil {
			extras = map[string]string{}
		}

		streamed := uint8(0)
		if ev.Streamed {
			streamed = 1
		}

		err := batch.Append(
			ev.RequestID,
			ev.Source,
			ev.Timestamp,
			uint16(ev.Status),
			ev.DurationMs,
			streamed,
			ev.FinishReason,
			uint16(ev.Attempts),
			ev.ErrorKind,
			ev.ErrorMessage,
			upStart,
			upRespStart,
			upRespEnd,
			ev.RelayKeyHash,
			ev.PolicyID,
			ev.ModelID,
			ev.RequestedModel,
			ev.HostID,
			ev.HostKeyID,
			tokens,
			extras,
		)
		if err != nil {
			return fmt.Errorf("append row: %w", err)
		}
	}

	return batch.Send()
}

// Events returns raw usage events matching q, newest-first.
func (s *Sink) Events(ctx context.Context, q usage.EventQuery) ([]usage.Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = usage.DefaultEventLimit
	}
	if limit > usage.MaxEventLimit {
		limit = usage.MaxEventLimit
	}

	where, args := buildWhere(q, false)

	sql := fmt.Sprintf(
		"SELECT request_id, source, ts, status, duration_ms, streamed, finish_reason, attempts, error_kind, error_message, upstream_start, upstream_response_start, upstream_response_end, relay_key_hash, policy_id, model_id, requested_model, host_id, host_key_id, tokens, extras FROM %s%s ORDER BY ts DESC LIMIT %d",
		chTable, where, limit,
	)

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("usage/clickhouse: events query: %w", err)
	}
	defer rows.Close()

	var events []usage.Event
	for rows.Next() {
		var (
			ev          usage.Event
			streamed    uint8
			status      uint16
			attempts    uint16
			upStart     int64
			upRespStart int64
			upRespEnd   int64
			tokens      map[string]int64
			extras      map[string]string
		)
		if err := rows.Scan(
			&ev.RequestID,
			&ev.Source,
			&ev.Timestamp,
			&status,
			&ev.DurationMs,
			&streamed,
			&ev.FinishReason,
			&attempts,
			&ev.ErrorKind,
			&ev.ErrorMessage,
			&upStart,
			&upRespStart,
			&upRespEnd,
			&ev.RelayKeyHash,
			&ev.PolicyID,
			&ev.ModelID,
			&ev.RequestedModel,
			&ev.HostID,
			&ev.HostKeyID,
			&tokens,
			&extras,
		); err != nil {
			return nil, fmt.Errorf("usage/clickhouse: scan event: %w", err)
		}
		ev.Status = int(status)
		ev.Streamed = streamed == 1
		ev.Attempts = int(attempts)
		ev.Tokens = usage.Tokens(tokens)
		ev.Extras = extras
		if upStart != -1 {
			ev.Upstream = &usage.UpstreamTiming{
				Start:         upStart,
				ResponseStart: upRespStart,
				ResponseEnd:   upRespEnd,
			}
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// Summary returns aggregated rows grouped by q.GroupBy.
func (s *Sink) Summary(ctx context.Context, q usage.SummaryQuery) (usage.SummaryResult, error) {
	groupBy := q.GroupBy
	if groupBy == "" {
		groupBy = "source"
	}
	if !usage.IsValidGroupBy(groupBy) {
		return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: invalid groupBy %q", groupBy)
	}

	col := groupBy // column name matches the GroupBy value in this schema

	where, args := buildWhere(q.EventQuery, true)

	sql := fmt.Sprintf(`
SELECT
    %s,
    count()                             AS requests,
    countIf(status >= 400)              AS error_count,
    sumMap(tokens)                      AS tokens,
    toInt64(avg(duration_ms))           AS avg_ms,
    toInt64(quantile(0.5)(duration_ms)) AS p50,
    toInt64(quantile(0.95)(duration_ms)) AS p95,
    toInt64(quantile(0.99)(duration_ms)) AS p99,
    max(duration_ms)                    AS max_ms,
    min(ts)                             AS first_seen,
    max(ts)                             AS last_seen
FROM %s%s
GROUP BY %s
ORDER BY requests DESC`,
		col, chTable, where, col)

	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: summary query: %w", err)
	}
	defer rows.Close()

	var result usage.SummaryResult
	for rows.Next() {
		var (
			groupVal             string
			requests             int64
			errCount             int64
			tokens               map[string]int64
			avgMs                int64
			p50, p95, p99, maxMs int64
			firstSeen, lastSeen  time.Time
		)
		if err := rows.Scan(&groupVal, &requests, &errCount, &tokens, &avgMs, &p50, &p95, &p99, &maxMs, &firstSeen, &lastSeen); err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/clickhouse: scan summary: %w", err)
		}
		result.Rows = append(result.Rows, usage.SummaryRow{
			Group:      map[string]string{groupBy: groupVal},
			Requests:   requests,
			ErrorCount: errCount,
			Tokens:     tokens,
			DurationMs: usage.DurationStats{
				Avg: avgMs,
				P50: p50,
				P95: p95,
				P99: p99,
				Max: maxMs,
			},
			FirstSeen: firstSeen,
			LastSeen:  lastSeen,
		})
		if result.From.IsZero() || firstSeen.Before(result.From) {
			result.From = firstSeen
		}
		if lastSeen.After(result.To) {
			result.To = lastSeen
		}
	}
	return result, rows.Err()
}

// buildWhere generates the WHERE clause and positional args for an EventQuery.
// The forSummary flag has no effect on output currently; it is reserved for
// future divergence between the two query paths.
func buildWhere(q usage.EventQuery, _ bool) (string, []any) {
	var clauses []string
	var args []any

	if q.Since > 0 {
		clauses = append(clauses, fmt.Sprintf("ts >= now() - INTERVAL %d SECOND", int64(q.Since.Seconds())))
	}
	if q.RelayKeyHash != "" {
		clauses = append(clauses, "relay_key_hash = ?")
		args = append(args, q.RelayKeyHash)
	}
	if q.PolicyID != "" {
		clauses = append(clauses, "policy_id = ?")
		args = append(args, q.PolicyID)
	}
	if q.ModelID != "" {
		clauses = append(clauses, "model_id = ?")
		args = append(args, q.ModelID)
	}
	if q.HostID != "" {
		clauses = append(clauses, "host_id = ?")
		args = append(args, q.HostID)
	}
	if q.Source != "" {
		clauses = append(clauses, "source = ?")
		args = append(args, q.Source)
	}
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

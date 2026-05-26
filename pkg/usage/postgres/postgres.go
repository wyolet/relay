// Package postgres implements usage.Sink, usage.Reader, and usage.Closer
// backed by PostgreSQL. Events are buffered in-memory and flushed in batches
// via pgx CopyFrom. PostgreSQL is its own durable store (its own WAL); there
// is no secondary JSONL segment queue here. On flush error the batch is
// dropped and counted in Dropped() — best-effort, no re-buffer.
//
// Expected kv ops per request: none (all async). One CopyFrom per flush
// interval across BatchSize events.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wyolet/relay/pkg/usage"
)

// Compile-time interface assertions.
var _ usage.Sink = (*Sink)(nil)
var _ usage.Reader = (*Sink)(nil)
var _ usage.Closer = (*Sink)(nil)

// Config holds all tunables for the PostgreSQL sink.
type Config struct {
	// DSN is the PostgreSQL connection string (postgres://user:pass@host/db).
	DSN string

	// RetentionDays controls how long rows are kept by the daily prune job.
	// <=0 disables pruning. Default 90.
	RetentionDays int

	// BatchSize is the number of events to accumulate before flushing.
	// Default 500.
	BatchSize int

	// FlushInterval is how often buffered events are flushed even if
	// BatchSize is not reached. Default 2s.
	FlushInterval time.Duration

	// Table is the table name. Must match ^[a-z_][a-z0-9_]*$.
	// Default "usage_events".
	Table string
}

func (c *Config) applyDefaults() {
	if c.RetentionDays <= 0 {
		c.RetentionDays = 90
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 2 * time.Second
	}
	if c.Table == "" {
		c.Table = "usage_events"
	}
}

var tableNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// expectedColumns is the set of columns insertBatch writes (and ensureSchema
// validates). Order matches the CopyFrom source row.
var expectedColumns = []string{
	"request_id", "source", "ts", "status", "duration_ms", "streamed",
	"finish_reason", "attempts", "error_kind", "error_message",
	"upstream_start", "upstream_response_start", "upstream_response_end",
	"relay_key_hash", "policy_id", "model_id", "requested_model",
	"host_id", "host_key_id", "tokens", "extras",
}

// createTableSQL creates the usage_events table. upstream_* are NULLABLE bigint
// (real NULL when Upstream==nil). tokens and extras are jsonb.
const createTableSQL = `CREATE TABLE IF NOT EXISTS %s (
    request_id               text         NOT NULL,
    source                   text         NOT NULL DEFAULT '',
    ts                       timestamptz  NOT NULL,
    status                   smallint     NOT NULL DEFAULT 0,
    duration_ms              bigint       NOT NULL DEFAULT 0,
    streamed                 boolean      NOT NULL DEFAULT false,
    finish_reason            text         NOT NULL DEFAULT '',
    attempts                 smallint     NOT NULL DEFAULT 0,
    error_kind               text         NOT NULL DEFAULT '',
    error_message            text         NOT NULL DEFAULT '',
    upstream_start           bigint,
    upstream_response_start  bigint,
    upstream_response_end    bigint,
    relay_key_hash           text         NOT NULL DEFAULT '',
    policy_id                text         NOT NULL DEFAULT '',
    model_id                 text         NOT NULL DEFAULT '',
    requested_model          text         NOT NULL DEFAULT '',
    host_id                  text         NOT NULL DEFAULT '',
    host_key_id              text         NOT NULL DEFAULT '',
    tokens                   jsonb        NOT NULL DEFAULT '{}',
    extras                   jsonb        NOT NULL DEFAULT '{}'
)`

const createIndexSQL = `
CREATE INDEX IF NOT EXISTS %s_ts_idx ON %s (ts DESC);
CREATE INDEX IF NOT EXISTS %s_model_idx ON %s (model_id, ts DESC);
CREATE INDEX IF NOT EXISTS %s_policy_idx ON %s (policy_id, ts DESC);
`

// ensureSchema creates the table + indexes if absent, then validates columns.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool, table string) error {
	ddl := fmt.Sprintf(createTableSQL, table)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("usage/postgres: create table: %w", err)
	}

	idx := fmt.Sprintf(createIndexSQL, table, table, table, table, table, table)
	if _, err := pool.Exec(ctx, idx); err != nil {
		return fmt.Errorf("usage/postgres: create indexes: %w", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_name = $1`, table)
	if err != nil {
		return fmt.Errorf("usage/postgres: describe %s: %w", table, err)
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("usage/postgres: scan column: %w", err)
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
			"usage/postgres: table %q exists with an incompatible schema (missing columns: %s) — drop or rename it so relay can create the current schema",
			table, strings.Join(missing, ", "))
	}
	return nil
}

// Sink is the PostgreSQL-backed implementation of usage.Sink, usage.Reader,
// and usage.Closer.
type Sink struct {
	pool    *pgxpool.Pool
	cfg     Config
	log     *slog.Logger
	dropped atomic.Uint64

	mu     sync.Mutex
	buf    []usage.Event
	stopCh chan struct{}
	doneCh chan struct{}
}

// New opens a pgxpool connection, validates the table name, ensures the
// schema exists, and starts the background flush ticker.
func New(ctx context.Context, cfg Config) (*Sink, error) {
	cfg.applyDefaults()

	if !tableNameRe.MatchString(cfg.Table) {
		return nil, fmt.Errorf("usage/postgres: invalid table name %q (must match ^[a-z_][a-z0-9_]*$)", cfg.Table)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("usage/postgres: parse DSN: %w", err)
	}
	poolCfg.MaxConns = 4
	poolCfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("usage/postgres: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("usage/postgres: ping: %w", err)
	}

	if err := ensureSchema(pingCtx, pool, cfg.Table); err != nil {
		pool.Close()
		return nil, err
	}

	s := &Sink{
		pool:   pool,
		cfg:    cfg,
		log:    slog.Default(),
		buf:    make([]usage.Event, 0, cfg.BatchSize),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	go s.flushLoop()
	return s, nil
}

// Write appends ev to the in-memory buffer. When BatchSize is reached, a
// synchronous flush is triggered inline (caller is the Emitter goroutine,
// not the hot path).
func (s *Sink) Write(ev usage.Event) error {
	s.mu.Lock()
	s.buf = append(s.buf, ev)
	flush := len(s.buf) >= s.cfg.BatchSize
	var batch []usage.Event
	if flush {
		batch = s.buf
		s.buf = make([]usage.Event, 0, s.cfg.BatchSize)
	}
	s.mu.Unlock()

	if flush {
		if err := s.insertBatch(batch); err != nil {
			s.log.Warn("usage/postgres: flush on full batch", "err", err, "dropped", len(batch))
			s.dropped.Add(uint64(len(batch)))
		}
	}
	return nil
}

// Close flushes any remaining buffered events and closes the pool.
func (s *Sink) Close() error {
	close(s.stopCh)
	<-s.doneCh
	pool := s.pool
	pool.Close()
	return nil
}

// Dropped returns the number of events dropped due to flush errors.
func (s *Sink) Dropped() uint64 {
	return s.dropped.Load()
}

// flushLoop ticks every FlushInterval and flushes the buffer. It also runs
// a daily prune if RetentionDays > 0.
func (s *Sink) flushLoop() {
	defer close(s.doneCh)

	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	// Prune at a distinct interval; daily is fine but we start after first
	// flush to avoid delaying startup.
	var lastPrune time.Time

	for {
		select {
		case <-s.stopCh:
			s.flushOnce()
			return
		case <-ticker.C:
			s.flushOnce()
			if s.cfg.RetentionDays > 0 {
				if time.Since(lastPrune) > 24*time.Hour {
					s.prune()
					lastPrune = time.Now()
				}
			}
		}
	}
}

func (s *Sink) flushOnce() {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.buf
	s.buf = make([]usage.Event, 0, s.cfg.BatchSize)
	s.mu.Unlock()

	if err := s.insertBatch(batch); err != nil {
		s.log.Warn("usage/postgres: flush", "err", err, "dropped", len(batch))
		s.dropped.Add(uint64(len(batch)))
	}
}

func (s *Sink) prune() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Now().UTC().AddDate(0, 0, -s.cfg.RetentionDays)
	_, err := s.pool.Exec(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE ts < $1", s.cfg.Table), cutoff)
	if err != nil {
		s.log.Warn("usage/postgres: prune", "err", err)
	}
}

// insertBatch copies events into Postgres using the binary COPY protocol.
func (s *Sink) insertBatch(events []usage.Event) error {
	if len(events) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows := make([][]any, 0, len(events))
	for _, ev := range events {
		// Nil maps marshal as JSON null; default to {} so jsonb_each_text is safe.
		tokens := ev.Tokens
		if tokens == nil {
			tokens = usage.Tokens{}
		}
		extras := ev.Extras
		if extras == nil {
			extras = map[string]string{}
		}
		tokensJSON, err := json.Marshal(tokens)
		if err != nil {
			tokensJSON = []byte("{}")
		}
		extrasJSON, err := json.Marshal(extras)
		if err != nil {
			extrasJSON = []byte("{}")
		}

		var upStart, upRespStart, upRespEnd *int64
		if ev.Upstream != nil {
			s1, s2, s3 := ev.Upstream.Start, ev.Upstream.ResponseStart, ev.Upstream.ResponseEnd
			upStart, upRespStart, upRespEnd = &s1, &s2, &s3
		}

		rows = append(rows, []any{
			ev.RequestID,
			ev.Source,
			ev.Timestamp,
			int16(ev.Status),
			ev.DurationMs,
			ev.Streamed,
			ev.FinishReason,
			int16(ev.Attempts),
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
			tokensJSON,
			extrasJSON,
		})
	}

	_, err := s.pool.CopyFrom(
		ctx,
		pgx.Identifier{s.cfg.Table},
		expectedColumns,
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("copy from: %w", err)
	}
	return nil
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

	where, args := buildWhere(q)
	sql := fmt.Sprintf(
		`SELECT request_id, source, ts, status, duration_ms, streamed,
		        finish_reason, attempts, error_kind, error_message,
		        upstream_start, upstream_response_start, upstream_response_end,
		        relay_key_hash, policy_id, model_id, requested_model,
		        host_id, host_key_id, tokens, extras
		 FROM %s%s ORDER BY ts DESC LIMIT %d`,
		s.cfg.Table, where, limit,
	)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("usage/postgres: events query: %w", err)
	}
	defer rows.Close()

	var events []usage.Event
	for rows.Next() {
		var (
			ev          usage.Event
			status      int16
			attempts    int16
			upStart     *int64
			upRespStart *int64
			upRespEnd   *int64
			tokensJSON  []byte
			extrasJSON  []byte
		)
		if err := rows.Scan(
			&ev.RequestID,
			&ev.Source,
			&ev.Timestamp,
			&status,
			&ev.DurationMs,
			&ev.Streamed,
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
			&tokensJSON,
			&extrasJSON,
		); err != nil {
			return nil, fmt.Errorf("usage/postgres: scan event: %w", err)
		}
		ev.Status = int(status)
		ev.Attempts = int(attempts)

		if upStart != nil {
			ev.Upstream = &usage.UpstreamTiming{
				Start:         *upStart,
				ResponseStart: *upRespStart,
				ResponseEnd:   *upRespEnd,
			}
		}

		if len(tokensJSON) > 0 {
			var tokens map[string]int64
			_ = json.Unmarshal(tokensJSON, &tokens)
			ev.Tokens = usage.Tokens(tokens)
		}
		if len(extrasJSON) > 0 {
			var extras map[string]string
			_ = json.Unmarshal(extrasJSON, &extras)
			ev.Extras = extras
		}

		events = append(events, ev)
	}
	return events, rows.Err()
}

// Summary returns aggregated rows grouped by q.GroupBy. Token sums are
// computed by unnesting the jsonb map in SQL; latency percentiles use
// percentile_cont. Both queries share the same WHERE and are merged in Go.
func (s *Sink) Summary(ctx context.Context, q usage.SummaryQuery) (usage.SummaryResult, error) {
	groupBy := q.GroupBy
	if groupBy == "" {
		groupBy = "source"
	}
	if !usage.IsValidGroupBy(groupBy) {
		return usage.SummaryResult{}, fmt.Errorf("usage/postgres: invalid groupBy %q", groupBy)
	}

	where, args := buildWhere(q.EventQuery)

	// Query 1: scalar aggregates + latency percentiles.
	scalarSQL := fmt.Sprintf(`
SELECT
    %s                                                       AS grp,
    count(*)::bigint                                         AS requests,
    count(*) FILTER (WHERE status >= 400)::bigint            AS error_count,
    avg(duration_ms)::bigint                                 AS avg_ms,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p50,
    percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p95,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)::bigint AS p99,
    max(duration_ms)                                         AS max_ms,
    min(ts)                                                  AS first_seen,
    max(ts)                                                  AS last_seen
FROM %s%s
GROUP BY %s
ORDER BY requests DESC`,
		groupBy, s.cfg.Table, where, groupBy,
	)

	scalarRows, err := s.pool.Query(ctx, scalarSQL, args...)
	if err != nil {
		return usage.SummaryResult{}, fmt.Errorf("usage/postgres: summary scalar query: %w", err)
	}
	defer scalarRows.Close()

	type scalarRow struct {
		grp        string
		requests   int64
		errCount   int64
		avgMs      int64
		p50, p95   int64
		p99, maxMs int64
		first      time.Time
		last       time.Time
	}
	var scalars []scalarRow
	for scalarRows.Next() {
		var r scalarRow
		if err := scalarRows.Scan(&r.grp, &r.requests, &r.errCount, &r.avgMs, &r.p50, &r.p95, &r.p99, &r.maxMs, &r.first, &r.last); err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/postgres: scan summary scalar: %w", err)
		}
		scalars = append(scalars, r)
	}
	if err := scalarRows.Err(); err != nil {
		return usage.SummaryResult{}, err
	}

	groupIdx := make(map[string]int, len(scalars))
	rows := make([]usage.SummaryRow, len(scalars))
	for i, r := range scalars {
		groupIdx[r.grp] = i
		rows[i] = usage.SummaryRow{
			Group:      map[string]string{groupBy: r.grp},
			Requests:   r.requests,
			ErrorCount: r.errCount,
			Tokens:     map[string]int64{},
			DurationMs: usage.DurationStats{Avg: r.avgMs, P50: r.p50, P95: r.p95, P99: r.p99, Max: r.maxMs},
			FirstSeen:  r.first,
			LastSeen:   r.last,
		}
	}

	// Query 2: token sums per group via jsonb_each_text. The filtered rows are
	// wrapped in a subquery so WHERE precedes the CROSS JOIN LATERAL.
	if len(scalars) > 0 {
		tokenSQL := fmt.Sprintf(`
SELECT sub.%s AS grp, kv.key, sum(kv.value::bigint)::bigint
FROM (SELECT %s, tokens FROM %s%s) AS sub
CROSS JOIN LATERAL jsonb_each_text(sub.tokens) AS kv
GROUP BY sub.%s, kv.key`,
			groupBy, groupBy, s.cfg.Table, where, groupBy,
		)
		tokenRows, err := s.pool.Query(ctx, tokenSQL, args...)
		if err != nil {
			return usage.SummaryResult{}, fmt.Errorf("usage/postgres: summary token query: %w", err)
		}
		defer tokenRows.Close()

		for tokenRows.Next() {
			var grp, key string
			var val int64
			if err := tokenRows.Scan(&grp, &key, &val); err != nil {
				return usage.SummaryResult{}, fmt.Errorf("usage/postgres: scan token row: %w", err)
			}
			if idx, ok := groupIdx[grp]; ok {
				rows[idx].Tokens[key] = val
			}
		}
		if err := tokenRows.Err(); err != nil {
			return usage.SummaryResult{}, err
		}
	}

	var result usage.SummaryResult
	result.Rows = rows
	for _, r := range rows {
		if result.From.IsZero() || r.FirstSeen.Before(result.From) {
			result.From = r.FirstSeen
		}
		if r.LastSeen.After(result.To) {
			result.To = r.LastSeen
		}
	}
	return result, nil
}

// buildWhere generates a WHERE clause and positional args ($1, $2, …) for an
// EventQuery. The clause is empty string when no filters are set.
func buildWhere(q usage.EventQuery) (string, []any) {
	var clauses []string
	var args []any
	n := 0

	add := func(clause string, v any) {
		n++
		clauses = append(clauses, fmt.Sprintf(clause, n))
		args = append(args, v)
	}

	if q.Since > 0 {
		add("ts >= now() - ($%d * interval '1 second')", q.Since.Seconds())
	}
	if q.RelayKeyHash != "" {
		add("relay_key_hash = $%d", q.RelayKeyHash)
	}
	if q.PolicyID != "" {
		add("policy_id = $%d", q.PolicyID)
	}
	if q.ModelID != "" {
		add("model_id = $%d", q.ModelID)
	}
	if q.HostID != "" {
		add("host_id = $%d", q.HostID)
	}
	if q.Source != "" {
		add("source = $%d", q.Source)
	}
	if q.StatusMin > 0 {
		add("status >= $%d", int16(q.StatusMin))
	}
	if q.StatusMax > 0 {
		add("status <= $%d", int16(q.StatusMax))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

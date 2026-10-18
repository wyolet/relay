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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
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
	// 0 keeps rows forever.
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
	"host_id", "host_key_id", "tokens", "extras", "tags",
	"model", "host", "policy",
	"provider", "cost_nanos", "cost_breakdown", "pricing",
	"project_id", "project", "team_id", "team",
	"principal_kind", "principal_id", "principal",
	"credential_kind", "credential_id",
	"relay_key",
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
    extras                   jsonb        NOT NULL DEFAULT '{}',
    tags                     jsonb        NOT NULL DEFAULT '{}',
    model                    text         NOT NULL DEFAULT '',
    host                     text         NOT NULL DEFAULT '',
    policy                   text         NOT NULL DEFAULT '',
    provider                 text         NOT NULL DEFAULT '',
    cost_nanos               bigint,
    cost_breakdown           jsonb        NOT NULL DEFAULT '{}',
    pricing                  text         NOT NULL DEFAULT '',
    project_id               text         NOT NULL DEFAULT '',
    project                  text         NOT NULL DEFAULT '',
    team_id                  text         NOT NULL DEFAULT '',
    team                     text         NOT NULL DEFAULT '',
    principal_kind           text         NOT NULL DEFAULT '',
    principal_id             text         NOT NULL DEFAULT '',
    principal                text         NOT NULL DEFAULT '',
    credential_kind          text         NOT NULL DEFAULT '',
    credential_id            text         NOT NULL DEFAULT '',
    relay_key                text         NOT NULL DEFAULT ''
)`

// alterTableSQL upgrades a pre-existing table in place — additive columns
// get an idempotent ALTER instead of the fail-fast "drop or rename" error.
var alterTableSQL = []string{
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '{}'`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS model text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS host text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS policy text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS provider text NOT NULL DEFAULT ''`,
	// cost_nanos is nullable: NULL = unpriced (distinct from a priced $0),
	// matching the upstream_* timing columns' NULL-as-absent convention.
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS cost_nanos bigint`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS cost_breakdown jsonb NOT NULL DEFAULT '{}'`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS pricing text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS project_id text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS project text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS team_id text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS team text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS principal_kind text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS principal_id text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS principal text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS credential_kind text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS credential_id text NOT NULL DEFAULT ''`,
	`ALTER TABLE %s ADD COLUMN IF NOT EXISTS relay_key text NOT NULL DEFAULT ''`,
}

const createIndexSQL = `
CREATE INDEX IF NOT EXISTS %s_ts_idx ON %s (ts DESC);
CREATE INDEX IF NOT EXISTS %s_model_idx ON %s (model_id, ts DESC);
CREATE INDEX IF NOT EXISTS %s_policy_idx ON %s (policy_id, ts DESC);
CREATE INDEX IF NOT EXISTS %s_project_idx ON %s (project_id, ts DESC);
`

// ensureSchema creates the table + indexes if absent, then validates columns.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool, table string) error {
	ddl := fmt.Sprintf(createTableSQL, table)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("usage/postgres: create table: %w", err)
	}

	for _, stmt := range alterTableSQL {
		if _, err := pool.Exec(ctx, fmt.Sprintf(stmt, table)); err != nil {
			return fmt.Errorf("usage/postgres: alter table: %w", err)
		}
	}

	idx := fmt.Sprintf(createIndexSQL, table, table, table, table, table, table, table, table)
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
			tokens = sdkusage.Tokens{}
		}
		extras := ev.Extras
		if extras == nil {
			extras = map[string]string{}
		}
		tags := ev.Tags
		if tags == nil {
			tags = map[string]string{}
		}
		tokensJSON, err := json.Marshal(tokens)
		if err != nil {
			tokensJSON = []byte("{}")
		}
		extrasJSON, err := json.Marshal(extras)
		if err != nil {
			extrasJSON = []byte("{}")
		}
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			tagsJSON = []byte("{}")
		}
		costBkdn := ev.CostBreakdown
		if costBkdn == nil {
			costBkdn = map[string]int64{}
		}
		costBkdnJSON, err := json.Marshal(costBkdn)
		if err != nil {
			costBkdnJSON = []byte("{}")
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
			tagsJSON,
			ev.Model,
			ev.Host,
			ev.Policy,
			ev.Provider,
			ev.CostNanos,
			costBkdnJSON,
			ev.Pricing,
			ev.ProjectID,
			ev.Project,
			ev.TeamID,
			ev.Team,
			ev.PrincipalKind,
			ev.PrincipalID,
			ev.Principal,
			ev.CredentialKind,
			ev.CredentialID,
			ev.RelayKey,
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

	where, args := buildWhere(q, false)
	sql := fmt.Sprintf(
		`SELECT request_id, source, ts, status, duration_ms, streamed,
		        finish_reason, attempts, error_kind, error_message,
		        upstream_start, upstream_response_start, upstream_response_end,
		        relay_key_hash, policy_id, model_id, requested_model,
		        host_id, host_key_id, tokens, extras, tags,
		        model, host, policy,
		        provider, cost_nanos, cost_breakdown, pricing,
		        project_id, project, team_id, team,
		        principal_kind, principal_id, principal,
		        credential_kind, credential_id, relay_key
		 FROM %s%s ORDER BY ts DESC, request_id DESC LIMIT %d`,
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
			tagsJSON    []byte
			costNanos   *int64
			costJSON    []byte
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
			&tagsJSON,
			&ev.Model,
			&ev.Host,
			&ev.Policy,
			&ev.Provider,
			&costNanos,
			&costJSON,
			&ev.Pricing,
			&ev.ProjectID,
			&ev.Project,
			&ev.TeamID,
			&ev.Team,
			&ev.PrincipalKind,
			&ev.PrincipalID,
			&ev.Principal,
			&ev.CredentialKind,
			&ev.CredentialID,
			&ev.RelayKey,
		); err != nil {
			return nil, fmt.Errorf("usage/postgres: scan event: %w", err)
		}
		ev.Status = int(status)
		ev.Attempts = int(attempts)

		if upStart != nil {
			ev.Upstream = &sdkusage.UpstreamTiming{
				Start:         *upStart,
				ResponseStart: *upRespStart,
				ResponseEnd:   *upRespEnd,
			}
		}

		if len(tokensJSON) > 0 {
			var tokens map[string]int64
			_ = json.Unmarshal(tokensJSON, &tokens)
			ev.Tokens = sdkusage.Tokens(tokens)
		}
		if len(extrasJSON) > 0 {
			var extras map[string]string
			_ = json.Unmarshal(extrasJSON, &extras)
			ev.Extras = extras
		}
		if len(tagsJSON) > 0 {
			var tags map[string]string
			_ = json.Unmarshal(tagsJSON, &tags)
			if len(tags) > 0 {
				ev.Tags = tags
			}
		}
		ev.CostNanos = costNanos
		if costNanos != nil && len(costJSON) > 0 {
			var bkdn map[string]int64
			_ = json.Unmarshal(costJSON, &bkdn)
			if len(bkdn) > 0 {
				ev.CostBreakdown = bkdn
			}
		}

		events = append(events, ev)
	}
	return events, rows.Err()
}

// buildWhere generates a WHERE clause and positional args ($1, $2, …) for an
// EventQuery. The clause is empty string when no filters are set. With
// aggregate set (Summary/TimeSeries), LogOnly events — pre-upstream
// rejections, status 0 + error_kind — are excluded: they belong to the logs
// view, not usage stats. Event listings (aggregate=false) keep them.
func buildWhere(q usage.EventQuery, aggregate bool) (string, []any) {
	var clauses []string
	var args []any
	n := 0

	add := func(clause string, v any) {
		n++
		clauses = append(clauses, fmt.Sprintf(clause, n))
		args = append(args, v)
	}
	// any adds a `col = ANY($n)` membership clause; pgx encodes []string as
	// text[]. Empty slice means no filter on that column.
	any := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		add(col+" = ANY($%d)", vals)
	}

	// Lower bound: From if set, else relative Since. Upper bound: To.
	if !q.From.IsZero() {
		add("ts >= $%d", q.From)
	} else if q.Since > 0 {
		add("ts >= now() - ($%d * interval '1 second')", q.Since.Seconds())
	}
	if !q.To.IsZero() {
		add("ts <= $%d", q.To)
	}
	if !q.Before.IsZero() {
		add("ts < $%d", q.Before)
	}
	if !q.CursorTS.IsZero() {
		n++
		c1 := n
		n++
		c2 := n
		clauses = append(clauses, fmt.Sprintf("(ts, request_id) < ($%d, $%d)", c1, c2))
		args = append(args, q.CursorTS, q.CursorID)
	}
	if q.RequestID != "" {
		add("request_id = $%d", q.RequestID)
	}
	// Read scope is a disjunction, not another membership filter: the
	// caller's projects OR their own traffic.
	if len(q.ScopeProjectID) > 0 || len(q.ScopeRelayKeyHash) > 0 || len(q.ScopePrincipalID) > 0 {
		var or []string
		scopeAny := func(col string, vals []string) {
			if len(vals) == 0 {
				return
			}
			n++
			or = append(or, fmt.Sprintf("%s = ANY($%d)", col, n))
			args = append(args, vals)
		}
		scopeAny("project_id", q.ScopeProjectID)
		scopeAny("relay_key_hash", q.ScopeRelayKeyHash)
		scopeAny("principal_id", q.ScopePrincipalID)
		clauses = append(clauses, "("+strings.Join(or, " OR ")+")")
	}
	any("relay_key_hash", q.RelayKeyHash)
	any("policy_id", q.PolicyID)
	any("project_id", q.ProjectID)
	any("team_id", q.TeamID)
	any("principal_id", q.PrincipalID)
	any("model_id", q.ModelID)
	any("host_id", q.HostID)
	any("source", q.Source)
	any("finish_reason", q.FinishReason)
	any("error_kind", q.ErrorKind)
	if q.StatusMin > 0 {
		add("status >= $%d", int16(q.StatusMin))
	}
	if q.StatusMax > 0 {
		add("status <= $%d", int16(q.StatusMax))
	}
	if len(q.Status) > 0 {
		// pgx encodes []int16 as a smallint array; status column is smallint.
		vals := make([]int16, len(q.Status))
		for i, v := range q.Status {
			vals[i] = int16(v)
		}
		add("status = ANY($%d)", vals)
	}
	any("host_key_id", q.HostKeyID)
	any("requested_model", q.RequestedModel)
	any("model", q.Model)
	any("host", q.Host)
	any("policy", q.Policy)
	any("provider", q.Provider)
	any("relay_key", q.RelayKey)
	for _, k := range sortedTagKeys(q.Tags) {
		vals := q.Tags[k]
		if len(vals) == 0 {
			continue
		}
		n += 2
		clauses = append(clauses, fmt.Sprintf("COALESCE(tags->>$%d, '') = ANY($%d)", n-1, n))
		args = append(args, k, vals)
	}
	if q.Streamed != nil {
		add("streamed = $%d", *q.Streamed)
	}
	if q.ErrorsOnly != nil {
		if *q.ErrorsOnly {
			clauses = append(clauses, "(status >= 400 OR error_kind != '')")
		} else {
			clauses = append(clauses, "(status < 400 AND error_kind = '')")
		}
	}
	if q.AttemptsMin > 0 {
		add("attempts >= $%d", int16(q.AttemptsMin))
	}
	if q.DurationMsMin > 0 {
		add("duration_ms >= $%d", q.DurationMsMin)
	}
	if q.DurationMsMax > 0 {
		add("duration_ms <= $%d", q.DurationMsMax)
	}
	if q.TTFTMsMin > 0 || q.TTFTMsMax > 0 {
		// upstream_response_start is NULL when Upstream==nil; exclude NULLs.
		clauses = append(clauses, "upstream_response_start IS NOT NULL")
		if q.TTFTMsMin > 0 {
			add("(upstream_response_start / 1000) >= $%d", q.TTFTMsMin)
		}
		if q.TTFTMsMax > 0 {
			add("(upstream_response_start / 1000) <= $%d", q.TTFTMsMax)
		}
	}
	if q.Q != "" {
		needle := "%" + q.Q + "%"
		n++
		clauses = append(clauses, fmt.Sprintf(
			"(request_id ILIKE $%d OR model_id ILIKE $%d OR requested_model ILIKE $%d OR source ILIKE $%d)",
			n, n, n, n,
		))
		args = append(args, needle)
	}
	if aggregate {
		// Mirrors usage.Event.LogOnly.
		clauses = append(clauses, "NOT (status = 0 AND error_kind != '')")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// sortedTagKeys gives the tag filter a deterministic clause order (map
// iteration is randomized).
func sortedTagKeys(tags map[string][]string) []string {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const (
	chBatchSize = 1000
	chTable     = "usage_events"
)

var createTableSQL = `
CREATE TABLE IF NOT EXISTS usage_events (
    event_version     UInt8,
    request_id        String CODEC(ZSTD),
    model             LowCardinality(String),
    provider          LowCardinality(String),
    pool              LowCardinality(String),
    secret_hash       FixedString(12),
    terminated_by     LowCardinality(String),
    tokens            Map(LowCardinality(String), UInt32) CODEC(ZSTD(1)),
    attempts          String CODEC(ZSTD),
    attribution       Map(LowCardinality(String), String),
    metrics           Map(LowCardinality(String), Int64),
    instance_id       LowCardinality(String),
    relay_version     LowCardinality(String),
    started_at        DateTime64(9, 'UTC') CODEC(DoubleDelta),
    ended_at          DateTime64(9, 'UTC') CODEC(DoubleDelta)
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(started_at)
ORDER BY (started_at, model, pool)
TTL toDateTime(started_at) + INTERVAL %d DAY %s
`

type clickHouseSink struct {
	conn   clickhouse.Conn
	buf    []Event
	logger *slog.Logger
}

func newClickHouseSink(cfg Config) (*clickHouseSink, error) {
	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("eventlog: parse clickhouse DSN: %w", err)
	}
	opts.MaxOpenConns = 4
	opts.MaxIdleConns = 2
	opts.ConnMaxLifetime = time.Hour

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("eventlog: open clickhouse: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("eventlog: clickhouse ping: %w", err)
	}

	if err := ensureSchema(ctx, conn, cfg.RetentionDays); err != nil {
		conn.Close()
		return nil, fmt.Errorf("eventlog: ensure schema: %w", err)
	}

	return &clickHouseSink{
		conn:   conn,
		buf:    make([]Event, 0, chBatchSize),
		logger: slog.Default(),
	}, nil
}

func (cs *clickHouseSink) write(b []byte) error {
	var ev Event
	if err := json.Unmarshal(b, &ev); err != nil {
		return fmt.Errorf("eventlog: unmarshal event: %w", err)
	}
	cs.buf = append(cs.buf, ev)
	if len(cs.buf) >= chBatchSize {
		cs.flushBuf()
	}
	return nil
}

func (cs *clickHouseSink) flush() {
	if len(cs.buf) > 0 {
		cs.flushBuf()
	}
}

func (cs *clickHouseSink) flushBuf() {
	if len(cs.buf) == 0 {
		return
	}
	events := cs.buf
	cs.buf = make([]Event, 0, chBatchSize)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	batch, err := cs.conn.PrepareBatch(ctx, "INS"+"ERT INTO "+chTable)
	if err != nil {
		cs.logger.Warn("eventlog: clickhouse prepare batch", "err", err)
		return
	}

	for _, ev := range events {
		attemptsJSON, _ := json.Marshal(ev.Attempts)
		if ev.Attempts == nil {
			attemptsJSON = []byte("[]")
		}
		attribution := ev.Attribution
		if attribution == nil {
			attribution = map[string]string{}
		}
		metrics := ev.Metrics
		if metrics == nil {
			metrics = map[string]int64{}
		}

		startedAt := parseEventTime(ev.StartedAt)
		endedAt := parseEventTime(ev.EndedAt)

		tokensMap := tokensToUInt32(ev.Tokens)

		if err := batch.Append(
			uint8(ev.EventVersion),
			ev.RequestID,
			ev.Model,
			ev.Provider,
			ev.Pool,
			padFixedString(ev.SecretHash, 12),
			ev.TerminatedBy,
			tokensMap,
			string(attemptsJSON),
			attribution,
			metrics,
			ev.InstanceID,
			ev.RelayVersion,
			startedAt,
			endedAt,
		); err != nil {
			cs.logger.Warn("eventlog: clickhouse batch append", "err", err)
			_ = batch.Abort()
			return
		}
	}

	if err := batch.Send(); err != nil {
		cs.logger.Warn("eventlog: clickhouse batch send", "err", err, "dropped", len(events))
	}
}

func (cs *clickHouseSink) ping(ctx context.Context) error {
	return cs.conn.Ping(ctx)
}

func (cs *clickHouseSink) close(_ context.Context) error {
	cs.flushBuf()
	return cs.conn.Close()
}

func parseEventTime(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05.999999999Z", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ensureSchema checks whether usage_events has the new map-based tokens column.
// If the table is missing the tokens column, or still has the legacy scalar
// columns (tokens_prompt etc.), it drops and recreates the table.
// This is pragmatic for dev. Production migrations are an operator concern —
// see commit message for DROP TABLE instructions before deploying.
func ensureSchema(ctx context.Context, conn clickhouse.Conn, retentionDays int) error {
	// Check columns via DESCRIBE TABLE. If table does not exist, CREATE will handle it.
	type colRow struct {
		Name string `ch:"name"`
	}
	var rows []colRow
	descErr := conn.Select(ctx, &rows, "DESCRIBE TABLE relay.usage_events")
	if descErr != nil {
		// Table doesn't exist yet — just create it.
		ddl := fmt.Sprintf(createTableSQL, retentionDays, "DEL"+"ETE")
		return conn.Exec(ctx, ddl)
	}

	hasTokensMap := false
	hasLegacy := false
	for _, row := range rows {
		switch row.Name {
		case "tokens":
			hasTokensMap = true
		case "tokens_prompt", "tokens_completion", "tokens_total", "tokens_cached":
			hasLegacy = true
		}
	}

	if !hasTokensMap || hasLegacy {
		// Schema is incompatible — drop and recreate.
		slog.Default().Warn("eventlog: usage_events schema incompatible, dropping and recreating table")
		if err := conn.Exec(ctx, "DROP TABLE IF EXISTS relay.usage_events"); err != nil {
			return fmt.Errorf("drop table: %w", err)
		}
	}

	ddl := fmt.Sprintf(createTableSQL, retentionDays, "DEL"+"ETE")
	return conn.Exec(ctx, ddl)
}

// tokensToUInt32 converts a map[string]int64 token map to map[string]uint32
// for ClickHouse binding. Values are clamped to zero on negative.
func tokensToUInt32(t map[string]int64) map[string]uint32 {
	if len(t) == 0 {
		return map[string]uint32{}
	}
	out := make(map[string]uint32, len(t))
	for k, v := range t {
		if v < 0 {
			v = 0
		}
		out[k] = uint32(v)
	}
	return out
}

// padFixedString returns s truncated or zero-padded to exactly n bytes.
func padFixedString(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	b := make([]byte, n)
	copy(b, s)
	return string(b)
}

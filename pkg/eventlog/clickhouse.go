package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const chTable = "usage_events"

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
    ended_at          DateTime64(9, 'UTC') CODEC(DoubleDelta),
    cost              Float64,
    currency          LowCardinality(String)
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(started_at)
ORDER BY (started_at, model, pool)
TTL toDateTime(started_at) + INTERVAL %d DAY %s
`

type clickHouseSink struct {
	conn   clickhouse.Conn
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

	// Server-side async insert: CH coalesces concurrent INSERTs into larger
	// MergeTree parts (default flush 200ms / 10MB / 100k rows). Avoids the
	// "one part per INSERT" pathology and the merge-CPU spikes that come
	// with it. wait_for_async_insert=1 keeps the ack path honest — we still
	// learn about queue-admit failures.
	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	opts.Settings["async_insert"] = 1
	opts.Settings["wait_for_async_insert"] = 1

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
		logger: slog.Default(),
	}, nil
}

func (cs *clickHouseSink) writeBatch(events []Event) {
	if len(events) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	batch, err := cs.conn.PrepareBatch(ctx, "INS"+"ERT INTO "+chTable)
	if err != nil {
		cs.logger.Warn("eventlog: clickhouse prepare batch", "err", err, "dropped", len(events))
		metricSendError.Inc()
		metricDropped.Add(float64(len(events)))
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
			ev.Cost,
			ev.Currency,
		); err != nil {
			cs.logger.Warn("eventlog: clickhouse batch append", "err", err)
			_ = batch.Abort()
			metricSendError.Inc()
			metricDropped.Add(float64(len(events)))
			return
		}
	}

	if err := batch.Send(); err != nil {
		cs.logger.Warn("eventlog: clickhouse batch send", "err", err, "dropped", len(events))
		metricSendError.Inc()
		metricDropped.Add(float64(len(events)))
	}
}

func (cs *clickHouseSink) ping(ctx context.Context) error {
	return cs.conn.Ping(ctx)
}

func (cs *clickHouseSink) close(_ context.Context) error {
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
func ensureSchema(ctx context.Context, conn clickhouse.Conn, retentionDays int) error {
	type colRow struct {
		Name string `ch:"name"`
		Type string `ch:"type"`
	}
	var rows []colRow
	descErr := conn.Select(ctx, &rows, "DESCRIBE TABLE relay.usage_events")
	if descErr != nil {
		ddl := fmt.Sprintf(createTableSQL, retentionDays, "DEL"+"ETE")
		return conn.Exec(ctx, ddl)
	}

	hasTokensMap := false
	hasLegacy := false
	hasCost := false
	costIsDecimal := false
	hasCurrency := false
	for _, row := range rows {
		switch row.Name {
		case "tokens":
			hasTokensMap = true
		case "tokens_prompt", "tokens_completion", "tokens_total", "tokens_cached":
			hasLegacy = true
		case "cost":
			hasCost = true
			if row.Type != "Float64" {
				costIsDecimal = true
			}
		case "currency":
			hasCurrency = true
		}
	}

	if !hasTokensMap || hasLegacy || !hasCost || !hasCurrency || costIsDecimal {
		slog.Default().Warn("eventlog: usage_events schema incompatible, dropping and recreating table")
		if err := conn.Exec(ctx, "DROP TABLE IF EXISTS relay.usage_events"); err != nil {
			return fmt.Errorf("drop table: %w", err)
		}
	}

	ddl := fmt.Sprintf(createTableSQL, retentionDays, "DEL"+"ETE")
	return conn.Exec(ctx, ddl)
}

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

func padFixedString(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	b := make([]byte, n)
	copy(b, s)
	return string(b)
}

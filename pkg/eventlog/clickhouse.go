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
    tokens_prompt     UInt32,
    tokens_completion UInt32,
    tokens_total      UInt32,
    tokens_cached     UInt32,
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
TTL toDateTime(started_at) + INTERVAL %d DAY DELETE
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

	ddl := fmt.Sprintf(createTableSQL, cfg.RetentionDays)
	if err := conn.Exec(ctx, ddl); err != nil {
		conn.Close()
		return nil, fmt.Errorf("eventlog: create table: %w", err)
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

	batch, err := cs.conn.PrepareBatch(ctx, "INSERT INTO "+chTable)
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

		if err := batch.Append(
			uint8(ev.EventVersion),
			ev.RequestID,
			ev.Model,
			ev.Provider,
			ev.Pool,
			padFixedString(ev.SecretHash, 12),
			ev.TerminatedBy,
			uint32(ev.Tokens.Prompt),
			uint32(ev.Tokens.Completion),
			uint32(ev.Tokens.Total),
			uint32(ev.Tokens.Cached),
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

// padFixedString returns s truncated or zero-padded to exactly n bytes.
func padFixedString(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	b := make([]byte, n)
	copy(b, s)
	return string(b)
}

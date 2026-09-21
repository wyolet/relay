package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
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
	// zstd unless the DSN sets compress= explicitly: consecutive agent
	// requests repeat long prefixes, so block compression on batched
	// inserts cuts relay→CH bandwidth sharply.
	if opts.Compression == nil {
		opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionZSTD}
	}
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

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

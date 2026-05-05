package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrBufferFull    = errors.New("eventlog: buffer full")
	ErrMarshalFailed = errors.New("eventlog: marshal failed")
	ErrLoggerClosed  = errors.New("eventlog: logger closed")
)

// Backend selects the storage backend for the Logger.
type Backend string

const (
	BackendFile       Backend = "file"
	BackendClickHouse Backend = "clickhouse"
)

// TokenCounts holds prompt/completion/total/cached token counts.
type TokenCounts struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
	Cached     int64 `json:"cached,omitempty"`
}

// AttemptRecord records a single upstream call within a request.
type AttemptRecord struct {
	SecretHash string `json:"secret_hash"`
	Outcome    string `json:"outcome"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
}

// Event is the structured event type passed to Append.
// fileSink writes it as JSON; clickhouseSink uses typed column inserts.
type Event struct {
	EventVersion int               `json:"event_version"`
	RequestID    string            `json:"request_id"`
	Model        string            `json:"model"`
	Provider     string            `json:"provider"`
	Pool         string            `json:"pool"`
	SecretHash   string            `json:"secret_hash"`
	TerminatedBy string            `json:"terminated_by"`
	Tokens       TokenCounts       `json:"tokens"`
	Attempts     []AttemptRecord   `json:"attempts,omitempty"`
	Attribution  map[string]string `json:"attribution,omitempty"`
	Metrics      map[string]int64  `json:"metrics,omitempty"`
	InstanceID   string            `json:"instance_id"`
	RelayVersion string            `json:"relay_version"`
	StartedAt    string            `json:"started_at"`
	EndedAt      string            `json:"ended_at"`
}

// sink is the internal backend interface. It receives pre-marshaled JSON bytes.
type sink interface {
	write(b []byte) error
	flush()
	ping(ctx context.Context) error
	close(ctx context.Context) error
}

// Config tunes the Logger. Zero values fall back to defaults.
type Config struct {
	// Backend selects the storage backend. Defaults to BackendFile.
	Backend Backend

	// DSN is required when Backend == BackendClickHouse.
	DSN string

	// RetentionDays sets the TTL for ClickHouse rows. Defaults to 90.
	RetentionDays int

	// Dir is required when Backend == BackendFile (or empty Backend).
	Dir string

	BufferSize  int
	FlushPeriod time.Duration
	Clock       func() time.Time
}

type Stats struct {
	Written     uint64
	Dropped     uint64
	LastWriteAt time.Time
	CurrentFile string
}

// Logger appends events to the configured backend via a bounded async channel.
type Logger struct {
	cfg     Config
	sk      sink
	ch      chan []byte
	done    chan struct{}
	closed  atomic.Bool
	written atomic.Uint64
	dropped atomic.Uint64

	mu          sync.Mutex
	lastWriteAt time.Time
	currentFile string
}

// New constructs a Logger and starts the writer goroutine.
func New(cfg Config) (*Logger, error) {
	if cfg.Backend == "" {
		cfg.Backend = BackendFile
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.FlushPeriod <= 0 {
		cfg.FlushPeriod = 5 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 90
	}

	var sk sink
	switch cfg.Backend {
	case BackendFile:
		if cfg.Dir == "" {
			if d := os.Getenv("RELAY_EVENTLOG_DIR"); d != "" {
				cfg.Dir = d
			} else {
				cfg.Dir = "./events"
			}
		}
		fs, err := newFileSink(cfg)
		if err != nil {
			return nil, err
		}
		sk = fs
	case BackendClickHouse:
		if cfg.DSN == "" {
			return nil, fmt.Errorf("eventlog: DSN required for clickhouse backend")
		}
		cs, err := newClickHouseSink(cfg)
		if err != nil {
			return nil, err
		}
		sk = cs
	default:
		return nil, fmt.Errorf("eventlog: unknown backend %q", cfg.Backend)
	}

	l := &Logger{
		cfg:  cfg,
		sk:   sk,
		ch:   make(chan []byte, cfg.BufferSize),
		done: make(chan struct{}),
	}
	if fs, ok := sk.(*fileSink); ok {
		fs.setLogger(l)
	}
	go l.run()
	return l, nil
}

// Append marshals event to JSON and enqueues it. Never blocks.
func (l *Logger) Append(_ context.Context, event any) error {
	if l.closed.Load() {
		l.dropped.Add(1)
		return ErrLoggerClosed
	}

	b, err := json.Marshal(event)
	if err != nil {
		l.dropped.Add(1)
		return fmt.Errorf("%w: %v", ErrMarshalFailed, err)
	}

	select {
	case l.ch <- b:
		return nil
	default:
		l.dropped.Add(1)
		return ErrBufferFull
	}
}

// Ping checks connectivity to the backend. fileSink always returns nil.
func (l *Logger) Ping(ctx context.Context) error {
	return l.sk.ping(ctx)
}

// Stats returns a snapshot of writer state.
func (l *Logger) Stats() Stats {
	l.mu.Lock()
	lwAt := l.lastWriteAt
	cf := l.currentFile
	l.mu.Unlock()
	return Stats{
		Written:     l.written.Load(),
		Dropped:     l.dropped.Load(),
		LastWriteAt: lwAt,
		CurrentFile: cf,
	}
}

// Close drains remaining events and flushes the backend. Idempotent.
func (l *Logger) Close(ctx context.Context) error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(l.ch)

	select {
	case <-l.done:
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "eventlog: Close deadline exceeded, some events may be lost\n")
		return ctx.Err()
	}
	return l.sk.close(ctx)
}

func (l *Logger) run() {
	defer close(l.done)

	ticker := time.NewTicker(l.cfg.FlushPeriod)
	defer ticker.Stop()

	for {
		select {
		case b, ok := <-l.ch:
			if !ok {
				return
			}
			l.writeOne(b)
		case <-ticker.C:
			l.sk.flush()
		}
	}
}

func (l *Logger) writeOne(b []byte) {
	if err := l.sk.write(b); err != nil {
		l.dropped.Add(1)
		fmt.Fprintf(os.Stderr, "eventlog: write: %v\n", err)
		return
	}
	now := l.cfg.Clock().UTC()
	l.written.Add(1)
	l.mu.Lock()
	l.lastWriteAt = now
	l.mu.Unlock()
}

// setCurrentFile is called by fileSink to update the Stats field.
func (l *Logger) setCurrentFile(name string) {
	l.mu.Lock()
	l.currentFile = name
	l.mu.Unlock()
}

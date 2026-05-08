package eventlog

import (
	"context"
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

// TokenCounts is the token map type used in events. Keys are convention-based
// (input, output, cache_creation, cache_read, reasoning, …).
type TokenCounts = map[string]int64

// AttemptRecord records a single upstream call within a request.
type AttemptRecord struct {
	SecretHash string `json:"secret_hash"`
	Outcome    string `json:"outcome"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
}

// Event is the structured event type passed to Append.
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
	Cost         float64           `json:"cost,omitempty"`
	Currency     string            `json:"currency,omitempty"`
}

// sink is the internal backend interface. Implementations receive whole
// batches from the dedicated flusher goroutine and must be safe to call
// only from that single goroutine.
type sink interface {
	writeBatch(events []Event)
	ping(ctx context.Context) error
	close(ctx context.Context) error
}

// Config tunes the Logger. Zero values fall back to defaults.
type Config struct {
	Backend Backend

	// DSN is required when Backend == BackendClickHouse.
	DSN string

	// RetentionDays sets the TTL for ClickHouse rows. Defaults to 90.
	RetentionDays int

	// Dir is required when Backend == BackendFile.
	Dir string

	// BufferSize is the capacity of the intake channel. Default 1024.
	BufferSize int

	// BatchSize is the row count that triggers a flush. Default 500.
	BatchSize int

	// FlushPeriod is the max age of buffered events before a flush.
	// Default 5s.
	FlushPeriod time.Duration

	Clock func() time.Time
}

// Logger appends events to the configured backend via a bounded async pipeline.
//
// Pipeline:
//
//	Append → ch (chan Event, cap=BufferSize)
//	         └─► intake goroutine: accumulates into a slice; on BatchSize
//	             or FlushPeriod, swap-and-hand-off to flushCh
//	             └─► flushCh (chan []Event, cap=4)
//	                  └─► flusher goroutine: calls sk.writeBatch (network I/O)
//
// The intake goroutine never blocks on network I/O, so a slow backend cannot
// cause hot-path Append drops unless flushCh fills (visible via
// metricBatchDropped).
type Logger struct {
	cfg     Config
	sk      sink
	ch      chan Event
	flushCh chan []Event

	intakeDone  chan struct{}
	flusherDone chan struct{}

	closed atomic.Bool

	mu          sync.Mutex
	lastWriteAt time.Time
	currentFile string
}

// New constructs a Logger and starts intake + flusher goroutines.
func New(cfg Config) (*Logger, error) {
	if cfg.Backend == "" {
		cfg.Backend = BackendFile
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
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
			cfg.Dir = "./events"
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
		cfg:         cfg,
		sk:          sk,
		ch:          make(chan Event, cfg.BufferSize),
		flushCh:     make(chan []Event, 4),
		intakeDone:  make(chan struct{}),
		flusherDone: make(chan struct{}),
	}
	if fs, ok := sk.(*fileSink); ok {
		fs.setLogger(l)
	}
	go l.runIntake()
	go l.runFlusher()
	return l, nil
}

// Append enqueues ev. Never blocks. Returns ErrLoggerClosed if closed,
// or ErrBufferFull if the intake channel is at capacity.
func (l *Logger) Append(_ context.Context, ev Event) error {
	if l.closed.Load() {
		metricDropped.Inc()
		return ErrLoggerClosed
	}
	select {
	case l.ch <- ev:
		return nil
	default:
		metricDropped.Inc()
		return ErrBufferFull
	}
}

// Ping checks connectivity to the backend. fileSink always returns nil.
func (l *Logger) Ping(ctx context.Context) error {
	return l.sk.ping(ctx)
}

// Stats returns a snapshot of writer state.
func (l *Logger) Stats() (lastWriteAt time.Time, currentFile string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastWriteAt, l.currentFile
}

// Close drains and shuts down the pipeline. Idempotent.
func (l *Logger) Close(ctx context.Context) error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(l.ch)

	// Wait for intake to drain and close flushCh.
	select {
	case <-l.intakeDone:
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "eventlog: Close deadline exceeded during intake drain\n")
		return ctx.Err()
	}
	// Wait for flusher to drain pending batches.
	select {
	case <-l.flusherDone:
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "eventlog: Close deadline exceeded during flush drain\n")
		return ctx.Err()
	}
	return l.sk.close(ctx)
}

// runIntake owns the in-memory accumulator. It does NOT do network I/O;
// it hands off full batches to the flusher via flushCh.
func (l *Logger) runIntake() {
	defer close(l.intakeDone)
	defer close(l.flushCh)

	buf := make([]Event, 0, l.cfg.BatchSize)
	ticker := time.NewTicker(l.cfg.FlushPeriod)
	defer ticker.Stop()

	handoff := func() {
		if len(buf) == 0 {
			return
		}
		batch := buf
		buf = make([]Event, 0, l.cfg.BatchSize)
		select {
		case l.flushCh <- batch:
		default:
			// Flusher backed up — drop the whole batch rather than block intake.
			metricBatchDropped.Inc()
			metricDropped.Add(float64(len(batch)))
		}
	}

	for {
		select {
		case ev, ok := <-l.ch:
			if !ok {
				handoff()
				return
			}
			buf = append(buf, ev)
			if len(buf) >= l.cfg.BatchSize {
				handoff()
			}
		case <-ticker.C:
			handoff()
		}
	}
}

// runFlusher serializes all sink I/O.
func (l *Logger) runFlusher() {
	defer close(l.flusherDone)
	for batch := range l.flushCh {
		start := time.Now()
		l.sk.writeBatch(batch)
		metricFlushDuration.Observe(time.Since(start).Seconds())
		metricWritten.Add(float64(len(batch)))
		now := l.cfg.Clock().UTC()
		l.mu.Lock()
		l.lastWriteAt = now
		l.mu.Unlock()
	}
}

// setCurrentFile is called by fileSink to update the Stats field.
func (l *Logger) setCurrentFile(name string) {
	l.mu.Lock()
	l.currentFile = name
	l.mu.Unlock()
}

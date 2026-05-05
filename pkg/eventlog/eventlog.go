package eventlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrBufferFull   = errors.New("eventlog: buffer full")
	ErrMarshalFailed = errors.New("eventlog: marshal failed")
	ErrLoggerClosed  = errors.New("eventlog: logger closed")
)

// Config tunes the file backend. Zero values fall back to defaults.
type Config struct {
	Dir         string
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

// Logger appends JSON events to daily-rotated JSONL files.
type Logger struct {
	cfg     Config
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
	if cfg.Dir == "" {
		if d := os.Getenv("RELAY_EVENTLOG_DIR"); d != "" {
			cfg.Dir = d
		} else {
			cfg.Dir = "./events"
		}
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.FlushPeriod <= 0 {
		cfg.FlushPeriod = 250 * time.Millisecond
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: mkdir %s: %w", cfg.Dir, err)
	}

	l := &Logger{
		cfg:  cfg,
		ch:   make(chan []byte, cfg.BufferSize),
		done: make(chan struct{}),
	}
	go l.run()
	return l, nil
}

// Append marshals event and enqueues it. Never blocks. Returns an error if
// the event is dropped.
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

// Close drains remaining events and flushes/syncs the file. Idempotent.
func (l *Logger) Close(ctx context.Context) error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(l.ch)

	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "eventlog: Close deadline exceeded, some events may be lost\n")
		return ctx.Err()
	}
}

func (l *Logger) run() {
	defer close(l.done)

	var (
		f    *os.File
		bw   *bufio.Writer
		date string
	)

	openFile := func(d string) error {
		name := filepath.Join(l.cfg.Dir, "events-"+d+".jsonl")
		var err error
		f, err = os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		bw = bufio.NewWriter(f)
		date = d
		l.mu.Lock()
		l.currentFile = name
		l.mu.Unlock()
		return nil
	}

	closeFile := func() {
		if bw != nil {
			bw.Flush()
		}
		if f != nil {
			f.Sync()
			f.Close()
			f = nil
			bw = nil
		}
	}

	ticker := time.NewTicker(l.cfg.FlushPeriod)
	defer ticker.Stop()
	defer closeFile()

	write := func(b []byte) {
		now := l.cfg.Clock().UTC()
		d := now.Format("2006-01-02")
		if d != date {
			closeFile()
			if err := openFile(d); err != nil {
				fmt.Fprintf(os.Stderr, "eventlog: open file: %v\n", err)
				l.dropped.Add(1)
				return
			}
		}
		bw.Write(b)
		bw.WriteByte('\n')
		l.written.Add(1)
		l.mu.Lock()
		l.lastWriteAt = now
		l.mu.Unlock()
	}

	for {
		select {
		case b, ok := <-l.ch:
			if !ok {
				// Channel closed: drain is done (channel was already drained by range or select).
				// Flush is handled by defer closeFile.
				return
			}
			write(b)
		case <-ticker.C:
			if bw != nil {
				bw.Flush()
			}
		}
	}
}

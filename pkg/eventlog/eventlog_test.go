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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// makeTestEvent returns a minimal valid Event for use in unit tests.
func makeTestEvent(requestID string) Event {
	return Event{
		EventVersion: 1,
		RequestID:    requestID,
		Model:        "gpt-4o",
		Provider:     "openai",
		Pool:         "default",
		SecretHash:   "abc123def456",
		TerminatedBy: "clean",
		Tokens:       TokenCounts{Prompt: 10, Completion: 20, Total: 30},
		InstanceID:   "pod-1",
		RelayVersion: "dev",
		StartedAt:    time.Now().UTC().Format("2006-01-02T15:04:05.999999999Z"),
		EndedAt:      time.Now().UTC().Format("2006-01-02T15:04:05.999999999Z"),
	}
}

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	ids := []string{"req-a", "req-b", "req-c"}
	ctx := context.Background()
	for _, id := range ids {
		if err := l.Append(ctx, makeTestEvent(id)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Find the written file.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	f, _ := os.Open(filepath.Join(dir, entries[0].Name()))
	defer f.Close()

	sc := bufio.NewScanner(f)
	var i int
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if ev.RequestID != ids[i] {
			t.Errorf("line %d RequestID mismatch: got %q, want %q", i, ev.RequestID, ids[i])
		}
		i++
	}
	if i != 3 {
		t.Errorf("expected 3 lines, got %d", i)
	}
}

func TestConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Dir: dir, BufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Append(ctx, makeTestEvent(fmt.Sprintf("req-%d-%d", i, j)))
			}
		}(i)
	}
	wg.Wait()
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}

	written := testutil.ToFloat64(metricWritten)
	dropped := testutil.ToFloat64(metricDropped)
	if written+dropped < 10000 {
		t.Errorf("written(%v)+dropped(%v) < 10000", written, dropped)
	}
}

func TestBufferOverflow(t *testing.T) {
	// Use a tiny buffer and a writer that blocks to force drops.
	// We replace the channel with a blocked reader by constructing a Logger
	// manually without starting the goroutine, then verify ErrBufferFull.
	dir := t.TempDir()

	// Build a logger with bufSize=4 but intercept before the goroutine drains.
	// Instead: create with bufSize=4, immediately fill it, then send one more.
	l := &Logger{
		cfg: Config{
			Dir:         dir,
			BufferSize:  4,
			FlushPeriod: time.Hour,
			Clock:       time.Now,
		},
		ch:   make(chan []byte, 4),
		done: make(chan struct{}),
	}
	// Don't start the writer goroutine — channel fills immediately.
	ctx := context.Background()
	var gotFull bool
	for i := 0; i < 10; i++ {
		err := l.Append(ctx, makeTestEvent(fmt.Sprintf("req-%d", i)))
		if errors.Is(err, ErrBufferFull) {
			gotFull = true
		}
	}
	if !gotFull {
		t.Error("expected ErrBufferFull, got none")
	}
	if testutil.ToFloat64(metricDropped) == 0 {
		t.Error("expected dropped > 0")
	}
}

func TestDailyRotation(t *testing.T) {
	dir := t.TempDir()

	// Clock starts on day 1, then advances to day 2.
	var tick atomic.Int64
	day1 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	day2 := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC).UnixNano()
	tick.Store(day1)
	clock := func() time.Time { return time.Unix(0, tick.Load()) }

	l, err := New(Config{Dir: dir, Clock: clock, FlushPeriod: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	writtenBase := testutil.ToFloat64(metricWritten)
	l.Append(ctx, makeTestEvent("req-day1-a"))
	l.Append(ctx, makeTestEvent("req-day1-b"))

	// Wait until both day-1 events are written before advancing the clock.
	for i := 0; i < 100; i++ {
		if testutil.ToFloat64(metricWritten) >= writtenBase+2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Advance clock to day 2.
	tick.Store(day2)
	l.Append(ctx, makeTestEvent("req-day2-c"))
	l.Append(ctx, makeTestEvent("req-day2-d"))

	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files after rotation, got %d", len(entries))
	}

	countLines := func(name string) int {
		f, _ := os.Open(filepath.Join(dir, name))
		defer f.Close()
		sc := bufio.NewScanner(f)
		n := 0
		for sc.Scan() {
			n++
		}
		return n
	}

	for _, e := range entries {
		n := countLines(e.Name())
		if n != 2 {
			t.Errorf("file %s: expected 2 lines, got %d", e.Name(), n)
		}
	}
}

func TestCloseAfterClose(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Second Close is idempotent.
	if err := l.Close(ctx); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
	// Append after Close.
	if err := l.Append(ctx, makeTestEvent("req-after-close")); !errors.Is(err, ErrLoggerClosed) {
		t.Errorf("Append after Close: got %v, want ErrLoggerClosed", err)
	}
}

func TestZeroEventAppends(t *testing.T) {
	// A zero-value Event is valid JSON and must not produce an error.
	dir := t.TempDir()
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := l.Append(ctx, Event{}); err != nil {
		t.Errorf("Append(zero Event): unexpected error: %v", err)
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Note: metricDropped is a process-level counter; we just verify no error was returned above.


	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	f, _ := os.Open(filepath.Join(dir, entries[0].Name()))
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Error("expected one line in file")
	}
}

func TestStatsAccuracy(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		l.Append(ctx, makeTestEvent(fmt.Sprintf("req-%d", i)))
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	lastWriteAt, currentFile := l.Stats()
	if lastWriteAt.IsZero() {
		t.Error("LastWriteAt is zero")
	}
	if currentFile == "" {
		t.Error("CurrentFile is empty")
	}
}

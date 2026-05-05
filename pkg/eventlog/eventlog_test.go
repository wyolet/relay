package eventlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	type Evt struct {
		Name string
		Val  int
	}
	events := []Evt{{"a", 1}, {"b", 2}, {"c", 3}}
	ctx := context.Background()
	for _, e := range events {
		if err := l.Append(ctx, e); err != nil {
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
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if m["Name"] != events[i].Name {
			t.Errorf("line %d Name mismatch", i)
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
				l.Append(ctx, map[string]int{"g": i, "j": j})
			}
		}(i)
	}
	wg.Wait()
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}

	s := l.Stats()
	if s.Written+s.Dropped != 10000 {
		t.Errorf("written(%d)+dropped(%d) = %d, want 10000", s.Written, s.Dropped, s.Written+s.Dropped)
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
		err := l.Append(ctx, map[string]int{"i": i})
		if errors.Is(err, ErrBufferFull) {
			gotFull = true
		}
	}
	if !gotFull {
		t.Error("expected ErrBufferFull, got none")
	}
	if l.dropped.Load() == 0 {
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
	l.Append(ctx, map[string]string{"day": "1", "seq": "a"})
	l.Append(ctx, map[string]string{"day": "1", "seq": "b"})

	// Wait until both day-1 events are written before advancing the clock.
	for i := 0; i < 100; i++ {
		if l.written.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Advance clock to day 2.
	tick.Store(day2)
	l.Append(ctx, map[string]string{"day": "2", "seq": "c"})
	l.Append(ctx, map[string]string{"day": "2", "seq": "d"})

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
	if err := l.Append(ctx, "x"); !errors.Is(err, ErrLoggerClosed) {
		t.Errorf("Append after Close: got %v, want ErrLoggerClosed", err)
	}
}

func TestMarshalError(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	ch := make(chan int) // channels are not JSON-marshalable
	err = l.Append(ctx, ch)
	if !errors.Is(err, ErrMarshalFailed) {
		t.Errorf("expected ErrMarshalFailed, got %v", err)
	}
	if l.dropped.Load() != 1 {
		t.Errorf("expected dropped==1, got %d", l.dropped.Load())
	}

	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// No file should have been written.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		f, _ := os.Open(filepath.Join(dir, e.Name()))
		sc := bufio.NewScanner(f)
		if sc.Scan() {
			t.Errorf("expected empty file, found content in %s", e.Name())
		}
		f.Close()
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
		l.Append(ctx, map[string]int{"i": i})
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s := l.Stats()
	if s.Written != 5 {
		t.Errorf("Written = %d, want 5", s.Written)
	}
	if s.LastWriteAt.IsZero() {
		t.Error("LastWriteAt is zero")
	}
	if s.CurrentFile == "" {
		t.Error("CurrentFile is empty")
	}
}

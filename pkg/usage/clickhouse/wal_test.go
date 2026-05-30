package clickhouse

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

var testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func makeEvent(id string) usage.Event {
	return usage.Event{
		RequestID:  id,
		Source:     "pipeline",
		Timestamp:  time.Now().UTC(),
		Status:     200,
		DurationMs: 42,
	}
}

// newQueue builds a segmentQueue with a very short flush interval so tests
// can control timing via explicit Close / Recover calls.
func newQueue(t *testing.T, dir string, maxLines int, flushFn func([]usage.Event) error) *segmentQueue {
	t.Helper()
	q, err := newSegmentQueue(dir, maxLines, time.Hour, 1024, testLogger, flushFn)
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	return q
}

// countSegments returns how many segment-*.jsonl files exist in dir.
func countSegments(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "segment-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return len(matches)
}

func activeExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, activeName))
	return err == nil
}

// --- tests ---

func TestRotateOnMaxLines(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var received []usage.Event
	flushFn := func(evs []usage.Event) error {
		mu.Lock()
		received = append(received, evs...)
		mu.Unlock()
		return nil
	}

	q := newQueue(t, dir, 3, flushFn)

	for i := 0; i < 3; i++ {
		if err := q.Write(makeEvent(fmt.Sprintf("req-%d", i))); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	// After 3 writes (== maxLines), the active file should have been rotated.
	// A new active.jsonl exists (0 lines), and a segment file was created.
	if countSegments(t, dir) != 1 {
		t.Fatalf("expected 1 segment after maxLines reached, got %d", countSegments(t, dir))
	}

	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	got := len(received)
	mu.Unlock()
	if got != 3 {
		t.Fatalf("expected 3 flushed events, got %d", got)
	}
}

func TestTimerBasedRotationAndFlush(t *testing.T) {
	dir := t.TempDir()
	flushed := make(chan []usage.Event, 10)
	flushFn := func(evs []usage.Event) error {
		flushed <- evs
		return nil
	}

	// Short flush interval so the ticker fires quickly.
	q, err := newSegmentQueue(dir, 10000, 50*time.Millisecond, 1024, testLogger, flushFn)
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}

	q.Write(makeEvent("timer-1"))
	q.Write(makeEvent("timer-2"))

	// Wait for the ticker to fire and flush.
	select {
	case evs := <-flushed:
		if len(evs) != 2 {
			t.Fatalf("expected 2 events from timer flush, got %d", len(evs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for timer-based flush")
	}

	q.Close()
}

func TestFlushFnErrorPreservesSegment(t *testing.T) {
	dir := t.TempDir()
	callCount := 0
	flushFn := func(evs []usage.Event) error {
		callCount++
		return errors.New("ch unavailable")
	}

	q := newQueue(t, dir, 2, flushFn)

	// Write 2 events to trigger rotation (maxLines=2).
	q.Write(makeEvent("e1"))
	q.Write(makeEvent("e2"))

	// One segment should exist after rotation.
	if countSegments(t, dir) != 1 {
		t.Fatalf("expected 1 segment after rotation, got %d", countSegments(t, dir))
	}

	// Manually trigger flushPending; it should fail and NOT delete the segment.
	q.flushPending()

	if countSegments(t, dir) != 1 {
		t.Fatalf("segment was deleted despite flush error — must not delete on error")
	}
	if callCount == 0 {
		t.Fatal("flushFn was never called")
	}

	q.Close()
}

func TestRecoverDrainsLeftoverSegments(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var received []usage.Event
	flushFn := func(evs []usage.Event) error {
		mu.Lock()
		received = append(received, evs...)
		mu.Unlock()
		return nil
	}

	// First queue: write 2 events, trigger rotation, then close WITHOUT flushing
	// by simulating a crash — we just stop the ticker and don't call Close.
	q1, err := newSegmentQueue(dir, 2, time.Hour, 1024, testLogger, func(evs []usage.Event) error {
		return errors.New("simulate unavailable")
	})
	if err != nil {
		t.Fatalf("q1: %v", err)
	}
	q1.Write(makeEvent("crash-1"))
	q1.Write(makeEvent("crash-2")) // triggers rotation → segment-*.jsonl
	// Simulate crash: stop goroutine without flushing.
	q1.ticker.Stop()
	close(q1.stop)
	<-q1.done

	if countSegments(t, dir) < 1 {
		t.Fatal("expected at least one leftover segment before recovery")
	}

	// Second queue with working flushFn: Recover() should drain leftover segments.
	q2, err := newSegmentQueue(dir, 10000, time.Hour, 1024, testLogger, flushFn)
	if err != nil {
		t.Fatalf("q2: %v", err)
	}
	q2.Recover()

	mu.Lock()
	n := len(received)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 events recovered, got %d", n)
	}
	if countSegments(t, dir) != 0 {
		t.Fatalf("expected 0 segments after recovery, got %d", countSegments(t, dir))
	}

	q2.Close()
}

func TestRecoverRenamesLeftoverActive(t *testing.T) {
	dir := t.TempDir()

	// Write an active.jsonl directly (simulating a crash mid-active).
	activePath := filepath.Join(dir, activeName)
	if err := os.WriteFile(activePath, []byte("{\"request_id\":\"orphan\",\"source\":\"pipeline\",\"ts\":\"2024-01-01T00:00:00Z\",\"status\":200,\"duration_ms\":1}\n"), 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}

	var mu sync.Mutex
	var received []usage.Event
	flushFn := func(evs []usage.Event) error {
		mu.Lock()
		received = append(received, evs...)
		mu.Unlock()
		return nil
	}

	q, err := newSegmentQueue(dir, 10000, time.Hour, 1024, testLogger, flushFn)
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	// The constructor opens and counts lines in active.jsonl (1 line), so it
	// does NOT rename it — Recover() must do that.
	q.Recover()

	mu.Lock()
	n := len(received)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 orphan event recovered, got %d", n)
	}

	q.Close()
}

func TestMaxSegmentsDropsOldest(t *testing.T) {
	dir := t.TempDir()
	flushFn := func(evs []usage.Event) error {
		return errors.New("always fail")
	}

	// maxSegments=2, maxLines=1 so every write rotates.
	q, err := newSegmentQueue(dir, 1, time.Hour, 2, testLogger, flushFn)
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}

	// Write 4 events → 4 rotations → 4 segments before any flush.
	// flushPending is only called by ticker or Close; here we call it manually.
	for i := 0; i < 4; i++ {
		q.Write(makeEvent(fmt.Sprintf("m%d", i)))
		time.Sleep(2 * time.Millisecond) // ensure distinct timestamps
	}

	q.flushPending() // tries to flush; fails; but enforces maxSegments cap first.

	// After enforcement, ≤ maxSegments (2) segments remain.
	if got := countSegments(t, dir); got > 2 {
		t.Fatalf("expected ≤2 segments after maxSegments enforcement, got %d", got)
	}
	if q.Dropped() == 0 {
		t.Fatal("expected Dropped() > 0 after segment eviction")
	}

	q.Close()
}

func TestCloseFlushesFinalActiveSegment(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var received []usage.Event
	flushFn := func(evs []usage.Event) error {
		mu.Lock()
		received = append(received, evs...)
		mu.Unlock()
		return nil
	}

	q := newQueue(t, dir, 10000, flushFn)
	q.Write(makeEvent("final-1"))
	q.Write(makeEvent("final-2"))

	if err := q.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	n := len(received)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 events flushed on Close, got %d", n)
	}
	if activeExists(dir) {
		// After close the active file is rotated+flushed; it may be recreated
		// as empty by openActive inside rotateLocked. Check it has 0 lines.
		c, _ := countLines(filepath.Join(dir, activeName))
		if c != 0 {
			t.Fatalf("expected empty active after Close, got %d lines", c)
		}
	}
}

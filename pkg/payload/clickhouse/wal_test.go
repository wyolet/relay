package clickhouse

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
)

func rec(id string, body int) payload.Record {
	b := make([]byte, body)
	for i := range b {
		b[i] = 'x'
	}
	return payload.Record{RequestID: id, Timestamp: time.Now().UTC(), RequestBody: b}
}

// collector is a flushFn that records every flushed record.
type collector struct {
	mu  sync.Mutex
	got []payload.Record
}

func (c *collector) flush(recs []payload.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, recs...)
	return nil
}

func (c *collector) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.got))
	for i, r := range c.got {
		out[i] = r.RequestID
	}
	return out
}

func newQueue(t *testing.T, dir string, maxLines, maxBytes int, fn func([]payload.Record) error) *segmentQueue {
	t.Helper()
	q, err := newSegmentQueue(dir, maxLines, maxBytes, time.Hour, 256, testLogger(), fn)
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	return q
}

func TestWAL_RotateOnMaxLines(t *testing.T) {
	dir := t.TempDir()
	c := &collector{}
	q := newQueue(t, dir, 3, 1<<30, c.flush)
	defer q.Close()

	for i := 0; i < 7; i++ {
		if err := q.Write(rec(string(rune('a'+i)), 10)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// 7 writes, rotate every 3 → 2 full segments (6 records) flushed on the
	// next flushPending; the 7th sits in active.
	q.flushPending()
	if got := len(c.ids()); got != 6 {
		t.Fatalf("after line-rotation flush: want 6 flushed, got %d (%v)", got, c.ids())
	}
}

func TestWAL_RotateOnMaxBytes(t *testing.T) {
	dir := t.TempDir()
	c := &collector{}
	// maxLines huge, maxBytes small → byte threshold drives rotation. Each
	// record body is ~1KB; cap at 2KB so ~every 2nd write rotates.
	q := newQueue(t, dir, 1_000_000, 2048, c.flush)
	defer q.Close()

	for i := 0; i < 6; i++ {
		if err := q.Write(rec(string(rune('a'+i)), 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	q.flushPending()
	if got := len(c.ids()); got < 4 {
		t.Fatalf("byte-rotation should have flushed most records, got %d (%v)", got, c.ids())
	}
}

func TestWAL_RecoverReplaysLeftoverSegments(t *testing.T) {
	dir := t.TempDir()

	// First queue writes + rotates a couple segments, then "crashes" (Close
	// without the flushFn having drained — simulate by a flushFn that errors).
	failFn := func([]payload.Record) error { return errFlush }
	q1 := newQueue(t, dir, 2, 1<<30, failFn)
	for i := 0; i < 5; i++ {
		if err := q1.Write(rec(string(rune('a'+i)), 10)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = q1.Close() // flush fails → segments remain on disk

	// Sanity: segments are present.
	segs, _ := filepath.Glob(filepath.Join(dir, segmentGlob))
	if len(segs) == 0 {
		t.Fatal("expected leftover segments after failed flush")
	}

	// Second queue with a working flushFn recovers them.
	c := &collector{}
	q2 := newQueue(t, dir, 2, 1<<30, c.flush)
	defer q2.Close()
	q2.Recover()
	if got := len(c.ids()); got < 4 {
		t.Fatalf("recover should replay leftover segments, got %d (%v)", got, c.ids())
	}
}

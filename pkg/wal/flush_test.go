package wal

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// barrier releases its callers once n of them are waiting, and fails the test
// if they never are: proof that n flushes ran at the same time.
type barrier struct {
	t       *testing.T
	n       int
	mu      sync.Mutex
	waiting int
	full    chan struct{}
}

func newBarrier(t *testing.T, n int) *barrier {
	return &barrier{t: t, n: n, full: make(chan struct{})}
}

func (b *barrier) wait() {
	b.mu.Lock()
	b.waiting++
	if b.waiting == b.n {
		close(b.full)
	}
	b.mu.Unlock()
	select {
	case <-b.full:
	case <-time.After(5 * time.Second):
		b.t.Errorf("only %d of %d flushes ran at once", b.waiting, b.n)
	}
}

// pendingIDs returns the ids of the records still on disk.
func pendingIDs(t *testing.T, dir string) []string {
	t.Helper()
	var ids []string
	for _, content := range walFiles(t, dir) {
		for line := range strings.Lines(content) {
			var r testRecord
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, r.ID)
		}
	}
	return ids
}

func recordIDs(recs []testRecord) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	return ids
}

// collector is a fake flush that records what reached the sink.
type collector struct {
	mu  sync.Mutex
	ids []string
}

func (c *collector) add(recs []testRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, recordIDs(recs)...)
}

func (c *collector) sorted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(slices.Values(c.ids))
}

func TestFlushPendingFlushesSegmentsConcurrently(t *testing.T) {
	const workers = 4
	dir := t.TempDir()
	ready := newBarrier(t, workers)
	var sink collector
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 2, FlushWorkers: workers}, func(recs []testRecord) error {
		ready.wait()
		sink.add(recs)
		return nil
	})
	defer q.StopBackground()

	recs := testRecords(16)
	if err := q.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}
	q.FlushPending()

	if got, want := sink.sorted(), slices.Sorted(slices.Values(recordIDs(recs))); !slices.Equal(got, want) {
		t.Fatalf("sink received %q, want each record once: %q", got, want)
	}
	if left := pendingIDs(t, dir); len(left) != 0 {
		t.Fatalf("records %q left on disk after a successful pass", left)
	}
	if q.failing.Load() {
		t.Fatal("failing set after a successful pass")
	}
	if n := len(q.segmentLines); n != 0 {
		t.Fatalf("%d segment line counts left after the pass", n)
	}
}

// Once the sink refuses a segment, the pass starts no other: the in-flight
// ones finish, every refused segment stays on disk, and the next pass drains.
func TestFlushPendingStopsStartingSegmentsAfterFailure(t *testing.T) {
	const workers = 4
	dir := t.TempDir()
	ready := newBarrier(t, workers)
	var down atomic.Bool
	down.Store(true)
	var calls atomic.Int32
	var sink collector
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 2, FlushWorkers: workers}, func(recs []testRecord) error {
		if down.Load() {
			calls.Add(1)
			ready.wait()
			return errors.New("sink down")
		}
		sink.add(recs)
		return nil
	})
	defer q.StopBackground()

	recs := testRecords(24) // 12 segments
	if err := q.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}
	q.FlushPending()

	if got := calls.Load(); got != workers {
		t.Fatalf("flush called %d times on a down sink, want only the %d already in flight", got, workers)
	}
	if left := pendingIDs(t, dir); len(left) != len(recs) {
		t.Fatalf("%d of %d records on disk after a refused pass", len(left), len(recs))
	}
	if !q.failing.Load() {
		t.Fatal("failing not set after a refused pass")
	}

	down.Store(false)
	q.FlushPending()
	if got := sink.sorted(); len(got) != len(recs) {
		t.Fatalf("sink received %d records after recovery, want %d", len(got), len(recs))
	}
	if q.failing.Load() {
		t.Fatal("failing still set after a successful pass")
	}
}

// One refused segment out of many loses nothing: each record is either in the
// sink once or still on disk, and the refused segment is among those on disk.
func TestFlushPendingKeepsRefusedSegment(t *testing.T) {
	dir := t.TempDir()
	var sink collector
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 2, FlushWorkers: 4}, func(recs []testRecord) error {
		if recs[0].ID == "r0" {
			return errors.New("bad segment")
		}
		sink.add(recs)
		return nil
	})
	defer q.StopBackground()

	recs := testRecords(40)
	if err := q.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}
	q.FlushPending()

	left := pendingIDs(t, dir)
	if !slices.Contains(left, "r0") {
		t.Fatalf("the refused segment was removed; on disk: %q", left)
	}
	got := slices.Sorted(slices.Values(append(sink.sorted(), left...)))
	if want := slices.Sorted(slices.Values(recordIDs(recs))); !slices.Equal(got, want) {
		t.Fatalf("sink plus disk hold %q, want each record once: %q", got, want)
	}
	if !q.failing.Load() {
		t.Fatal("failing not set after a refused segment")
	}
}

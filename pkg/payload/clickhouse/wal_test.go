package clickhouse

import (
	"bytes"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/payload"
)

// collector is a flush function that records what reached the sink and whether two flushes ever overlapped.
type collector struct {
	mu       sync.Mutex
	got      []payload.Record
	inFlight int
	overlap  bool
}

func (c *collector) flush(recs []payload.Record) error {
	c.mu.Lock()
	c.inFlight++
	c.overlap = c.overlap || c.inFlight > 1
	c.mu.Unlock()
	time.Sleep(time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
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

func writeRecords(t *testing.T, q *segmentQueue, bodySize int) []payload.Record {
	t.Helper()
	var recs []payload.Record
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		rec := payload.Record{RequestID: id, Timestamp: time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC), RequestBody: bytes.Repeat([]byte("x"), bodySize)}
		if err := q.Write(rec); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// The queue's own behaviour is pinned in pkg/wal; this pins how the sink wires it: a segment closes at either cap and is flushed at once, one segment at a time and in order, without waiting for the tick.
func TestSegmentQueueFlushesEachFullSegment(t *testing.T) {
	for _, tc := range []struct {
		name               string
		maxLines, maxBytes int
		bodySize           int
	}{
		{"two lines per segment", 2, 1 << 30, 10},
		{"two 1.4 KiB lines cross the 2 KiB byte cap", 1_000_000, 2048, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c collector
			q, err := newSegmentQueue(t.TempDir(), tc.maxLines, tc.maxBytes, time.Hour, 256, testLogger(), c.flush)
			if err != nil {
				t.Fatalf("newSegmentQueue: %v", err)
			}
			recs := writeRecords(t, q, tc.bodySize)

			deadline := time.Now().Add(5 * time.Second)
			for !slices.Equal(c.ids(), []string{"a", "b", "c", "d"}) {
				if time.Now().After(deadline) {
					t.Fatalf("flushed before the tick = %v, want the two full segments [a b c d]", c.ids())
				}
				time.Sleep(time.Millisecond)
			}
			if err := q.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if diff := cmp.Diff(recs, c.got); diff != "" {
				t.Errorf("records flushed by Close (-written +flushed):\n%s", diff)
			}
			if c.overlap {
				t.Error("two segments were flushed at once")
			}
		})
	}
}

// Records a failing sink left on disk reach the next run's sink unchanged and in order.
func TestSegmentQueueRecoversUnflushedRecords(t *testing.T) {
	dir := t.TempDir()
	down, err := newSegmentQueue(dir, 2, 1<<30, time.Hour, 256, testLogger(), func([]payload.Record) error { return errFlush })
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	recs := writeRecords(t, down, 10)
	_ = down.Close()

	var c collector
	up, err := newSegmentQueue(dir, 2, 1<<30, time.Hour, 256, testLogger(), c.flush)
	if err != nil {
		t.Fatalf("newSegmentQueue: %v", err)
	}
	defer up.Close()
	if got := c.ids(); len(got) != 0 {
		t.Fatalf("flushed %v before Recover", got)
	}
	up.Recover()
	if diff := cmp.Diff(recs, c.got); diff != "" {
		t.Errorf("records recovered (-written +flushed):\n%s", diff)
	}
}

package wal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
)

type testRecord struct {
	ID    string            `json:"id"`
	N     int               `json:"n"`
	Notes string            `json:"notes,omitempty"`
	Tags  map[string]string `json:"tags,omitempty"`
}

func openTestQueue[T any](t testing.TB, cfg Config, flush func([]T) error) *Queue[T] {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = time.Hour
	}
	if cfg.Kind == "" {
		cfg.Kind = "test"
	}
	cfg.Logger = slog.New(slog.DiscardHandler)
	q, err := Open(cfg, flush)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return q
}

func noFlush[T any]([]T) error { return nil }

// walFiles returns the segment contents oldest-first, then the active file.
func walFiles(t *testing.T, dir string) []string {
	t.Helper()
	q := &Queue[testRecord]{cfg: Config{Dir: dir}}
	segs, err := q.listSegments()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range append(segs, filepath.Join(dir, ActiveName)) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func testRecords(n int) []testRecord {
	recs := make([]testRecord, n)
	for i := range recs {
		recs[i] = testRecord{ID: fmt.Sprintf("r%d", i), N: i, Notes: strings.Repeat("x", i%7*40)}
	}
	return recs
}

func TestWriteBatchMatchesPerRecordWrites(t *testing.T) {
	for _, cfg := range []Config{
		{MaxLines: 3},
		{MaxLines: 1},
		{MaxLines: 100, MaxBytes: 400},
	} {
		t.Run(fmt.Sprintf("lines=%d bytes=%d", cfg.MaxLines, cfg.MaxBytes), func(t *testing.T) {
			recs := testRecords(17)

			one := cfg
			one.Dir = t.TempDir()
			perRecord := openTestQueue(t, one, noFlush[testRecord])
			for _, r := range recs {
				if err := perRecord.Write(r); err != nil {
					t.Fatal(err)
				}
			}
			perRecord.StopBackground()

			many := cfg
			many.Dir = t.TempDir()
			batched := openTestQueue(t, many, noFlush[testRecord])
			if err := batched.WriteBatch(recs[:5]); err != nil {
				t.Fatal(err)
			}
			if err := batched.WriteBatch(recs[5:]); err != nil {
				t.Fatal(err)
			}
			batched.StopBackground()

			want, got := walFiles(t, one.Dir), walFiles(t, many.Dir)
			if len(want) < 3 {
				t.Fatalf("expected rotations inside the batch, got %d files", len(want))
			}
			if !slices.Equal(got, want) {
				t.Fatalf("batched WAL files differ from per-record writes:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// Once WriteBatch returns, every record is in the file, not in the buffer.
func TestWriteBatchFlushesBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 10_000}, noFlush[testRecord])
	defer q.StopBackground()

	if err := q.WriteBatch(testRecords(300)); err != nil {
		t.Fatal(err)
	}
	n, err := CountLines(filepath.Join(dir, ActiveName))
	if err != nil {
		t.Fatal(err)
	}
	if n != 300 {
		t.Fatalf("active segment holds %d lines after WriteBatch, want 300", n)
	}
}

func TestWriteBatchSkipsUnmarshalableRecord(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 10_000}, noFlush[any])
	defer q.StopBackground()

	err := q.WriteBatch([]any{1, func() {}, 2})
	if err == nil || !strings.Contains(err.Error(), "skipped 1 of 3") {
		t.Fatalf("err = %v, want the one bad record reported", err)
	}
	b, rerr := os.ReadFile(filepath.Join(dir, ActiveName))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(b) != "1\n2\n" {
		t.Fatalf("active = %q, want the two good records", b)
	}
}

func TestSegmentDropCountsLostRecords(t *testing.T) {
	const kind = "wal_drop_test"
	lost := func() float64 { return testutil.ToFloat64(metrics.RecordsLost.WithLabelValues(kind)) }
	before := lost()

	q := openTestQueue(t, Config{MaxLines: 2, MaxSegments: 1, Kind: kind},
		func([]testRecord) error { return errors.New("sink down") })
	defer q.StopBackground()

	// 6 records at 2 per segment = 3 segments; the cap keeps 1.
	if err := q.WriteBatch(testRecords(6)); err != nil {
		t.Fatal(err)
	}
	q.FlushPending()

	if got := q.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d segments, want 2", got)
	}
	if got := lost() - before; got != 4 {
		t.Fatalf("records_lost_total{kind=%q} rose by %v, want 4", kind, got)
	}
}

// countingWriter counts the write calls that reach the segment file.
type countingWriter struct {
	w      io.Writer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.w.Write(p)
}

// benchmarkQueue writes 10k records per iteration (in 512-record batches
// when batch is set) and reports the write calls that reached the file.
func benchmarkQueue(b *testing.B, batch bool) {
	const records = 10_000
	recs := make([]testRecord, records)
	for i := range recs {
		recs[i] = testRecord{
			ID:    fmt.Sprintf("019a0000-0000-7000-8000-%012d", i),
			N:     i,
			Notes: strings.Repeat("n", 900),
			Tags:  map[string]string{"team": "platform", "env": "prod"},
		}
	}
	writes := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		q := openTestQueue(b, Config{MaxLines: records + 1}, noFlush[testRecord])
		counter := &countingWriter{w: q.active}
		q.writer = bufio.NewWriterSize(counter, writeBufferSize)
		b.StartTimer()
		if batch {
			for start := 0; start < records; start += 512 {
				if err := q.WriteBatch(recs[start:min(start+512, records)]); err != nil {
					b.Fatal(err)
				}
			}
		} else {
			for j := range recs {
				if err := q.Write(recs[j]); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StopTimer()
		writes += counter.writes
		q.StopBackground()
		b.StartTimer()
	}
	b.ReportMetric(b.Elapsed().Seconds()*1e9/float64(b.N*records), "ns/record")
	b.ReportMetric(float64(writes)/float64(b.N), "writes/10k")
}

func BenchmarkQueueWrite(b *testing.B)      { benchmarkQueue(b, false) }
func BenchmarkQueueWriteBatch(b *testing.B) { benchmarkQueue(b, true) }

// With FlushOnRotate a full segment reaches the flush function without
// waiting for the tick, and after a failed flush rotations stop retrying.
func TestFlushOnRotate(t *testing.T) {
	flushed := make(chan int, 16)
	var fail atomic.Bool
	q := openTestQueue(t, Config{MaxLines: 2, FlushOnRotate: true}, func(recs []testRecord) error {
		if fail.Load() {
			flushed <- -1
			return errors.New("sink down")
		}
		flushed <- len(recs)
		return nil
	})
	defer q.StopBackground()

	if err := q.WriteBatch(testRecords(2)); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-flushed:
		if n != 2 {
			t.Fatalf("flushed %d records, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a rotated segment was not flushed before the tick")
	}

	fail.Store(true)
	if err := q.WriteBatch(testRecords(2)); err != nil {
		t.Fatal(err)
	}
	if n := <-flushed; n != -1 {
		t.Fatalf("flush = %d, want the failing attempt", n)
	}
	if err := q.WriteBatch(testRecords(4)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-flushed:
		t.Fatal("a rotation retried a failing flush before the tick")
	case <-time.After(200 * time.Millisecond):
	}
}

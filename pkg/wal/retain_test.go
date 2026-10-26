package wal

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// decodedTestRecord mirrors json.Unmarshal of a testRecord line: omitempty
// drops an empty Tags map, which decodes as nil.
func decodedTestRecord(r testRecord) (testRecord, bool) {
	if len(r.Tags) == 0 {
		r.Tags = nil
	}
	return r, true
}

func openRetainingTestQueue(t testing.TB, cfg Config, flush func([]testRecord) error) *Queue[testRecord] {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = time.Hour
	}
	cfg.Kind = "test"
	cfg.Logger = slog.New(slog.DiscardHandler)
	q, err := OpenRetaining(cfg, flush, decodedTestRecord)
	if err != nil {
		t.Fatalf("OpenRetaining: %v", err)
	}
	return q
}

// pendingSegments returns the rotated segment paths oldest-first, whether
// each is held in memory, and what each file decodes to.
func pendingSegments(t *testing.T, q *Queue[testRecord]) ([]string, []bool, [][]testRecord) {
	t.Helper()
	paths, err := q.listSegments()
	if err != nil {
		t.Fatal(err)
	}
	held := make([]bool, len(paths))
	decoded := make([][]testRecord, len(paths))
	for i, p := range paths {
		q.mu.Lock()
		held[i] = q.segments[p].records != nil
		q.mu.Unlock()
		decoded[i], err = readSegment[testRecord](p, 0, q.log)
		if err != nil {
			t.Fatal(err)
		}
	}
	return paths, held, decoded
}

func collectBatches(batches *[][]testRecord) func([]testRecord) error {
	return func(recs []testRecord) error {
		*batches = append(*batches, recs)
		return nil
	}
}

// A segment flushed from memory hands over exactly what its file decodes to,
// and one holding a record the encoder rewrote flushes from the file.
func TestFlushFromMemoryEqualsFile(t *testing.T) {
	var batches [][]testRecord
	q := openRetainingTestQueue(t, Config{MaxLines: 3}, collectBatches(&batches))
	defer q.StopBackground()

	recs := []testRecord{
		{ID: "plain", N: 1}, {ID: "empty-tags", Tags: map[string]string{}}, goldenRecords()[4],
		{ID: "a"}, {ID: "invalid-utf8", Notes: "a\xffb"}, {ID: "b"},
		{ID: "replacement-char", Tags: map[string]string{"k": "�"}}, {ID: "c"}, {ID: "d"},
		{ID: "invalid-key", Tags: map[string]string{"k\xc3": "v"}}, {ID: "e"}, {ID: "f"},
	}
	recs = append(recs, testRecords(6)...)
	if err := q.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}

	_, held, want := pendingSegments(t, q)
	if wantHeld := []bool{true, false, false, false, true, true}; !reflect.DeepEqual(held, wantHeld) {
		t.Fatalf("held in memory = %v, want %v", held, wantHeld)
	}
	q.FlushPending()

	if !reflect.DeepEqual(batches, want) {
		t.Fatalf("flushed %+v\nwant the files' records %+v", batches, want)
	}
	if q.retainedBytes != 0 {
		t.Fatalf("%d bytes still held after the flush", q.retainedBytes)
	}
}

// A line the reader skips as oversized sends its segment to disk, so the
// flush skips it too.
func TestOversizedLineFlushesFromDisk(t *testing.T) {
	var batches [][]testRecord
	q := openRetainingTestQueue(t, Config{MaxLines: 3}, collectBatches(&batches))
	defer q.StopBackground()

	recs := []testRecord{{ID: "before"}, {ID: "oversized", Notes: strings.Repeat("x", scanBuf)}, {ID: "after"}}
	if err := q.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}
	_, held, _ := pendingSegments(t, q)
	if len(held) != 1 || held[0] {
		t.Fatalf("held in memory = %v, want one segment read from disk", held)
	}
	q.FlushPending()

	want := [][]testRecord{{{ID: "before"}, {ID: "after"}}}
	if !reflect.DeepEqual(batches, want) {
		t.Fatalf("flushed %+v, want %+v", batches, want)
	}
}

// The first attempt flushes from memory; after it fails, the retry and every
// segment rotated during the outage read the file.
func TestFailedFlushRetriesFromDisk(t *testing.T) {
	var batches [][]testRecord
	fail := true
	q := openRetainingTestQueue(t, Config{MaxLines: 2}, func(recs []testRecord) error {
		batches = append(batches, recs)
		if fail {
			return errors.New("sink down")
		}
		return nil
	})
	defer q.StopBackground()

	if err := q.WriteBatch(testRecords(2)); err != nil {
		t.Fatal(err)
	}
	paths, held, _ := pendingSegments(t, q)
	if !held[0] {
		t.Fatal("the rotated segment is not held in memory")
	}
	// Rewrite the file so the batch shows which copy each attempt read.
	disk := []testRecord{{ID: "from-disk-0"}, {ID: "from-disk-1"}}
	var body []byte
	for _, r := range disk {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		body = append(append(body, b...), '\n')
	}
	if err := os.WriteFile(paths[0], body, 0o644); err != nil {
		t.Fatal(err)
	}

	q.FlushPending()
	if q.retainedBytes != 0 {
		t.Fatalf("%d bytes still held after the failed flush", q.retainedBytes)
	}
	during := testRecords(4)[2:]
	if err := q.WriteBatch(during); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := pendingSegments(t, q); held[1] {
		t.Fatal("a segment rotated while the sink is down is held in memory")
	}

	fail = false
	q.FlushPending()
	want := [][]testRecord{testRecords(2), disk, during}
	if !reflect.DeepEqual(batches, want) {
		t.Fatalf("flushed %+v\nwant memory, then the file, then the file: %+v", batches, want)
	}

	// The active segment opened during the outage still flushes from disk;
	// the one after it is held again.
	if err := q.WriteBatch(testRecords(4)); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := pendingSegments(t, q); !reflect.DeepEqual(held, []bool{false, true}) {
		t.Fatalf("held in memory = %v, want [false true]", held)
	}
}

// Segments rotated but never flushed before a crash, and the active segment,
// are recovered from disk by the next process.
func TestCrashBetweenRotateAndFlushRecoversFromDisk(t *testing.T) {
	dir := t.TempDir()
	crashed := openRetainingTestQueue(t, Config{Dir: dir, MaxLines: 4}, func([]testRecord) error {
		t.Fatal("the crashed queue flushed")
		return nil
	})
	recs := testRecords(10)
	recs[3].Tags = map[string]string{}
	if err := crashed.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}
	crashed.StopBackground()

	var got []testRecord
	q := openRetainingTestQueue(t, Config{Dir: dir, MaxLines: 4}, func(batch []testRecord) error {
		got = append(got, batch...)
		return nil
	})
	defer q.StopBackground()
	if q.retaining {
		t.Fatal("an active segment left by the crash is held in memory")
	}
	q.Recover()

	if len(got) != len(recs) {
		t.Fatalf("recovered %d records, want %d", len(got), len(recs))
	}
	for i := range recs {
		want, _ := decodedTestRecord(recs[i])
		if !reflect.DeepEqual(got[i], want) {
			t.Fatalf("record %d = %+v, want %+v", i, got[i], want)
		}
	}
}

// Held records never exceed the bound; the segments past it flush from disk
// with the same records.
func TestRetainedRecordsStayWithinBound(t *testing.T) {
	var batches [][]testRecord
	q := openRetainingTestQueue(t, Config{MaxLines: 2}, collectBatches(&batches))
	defer q.StopBackground()
	q.retainLimit = 64 << 10

	notes := strings.Repeat("n", 4<<10)
	recs := make([]testRecord, 24)
	for i := range recs {
		recs[i] = testRecord{ID: fmt.Sprintf("r%d", i), N: i, Notes: notes}
	}
	for i := range recs {
		if err := q.Write(recs[i]); err != nil {
			t.Fatal(err)
		}
		if q.retainedBytes > q.retainLimit {
			t.Fatalf("%d bytes held after record %d, bound %d", q.retainedBytes, i, q.retainLimit)
		}
	}
	_, held, want := pendingSegments(t, q)
	var inMemory int
	for _, h := range held {
		if h {
			inMemory++
		}
	}
	if inMemory == 0 || inMemory == len(held) {
		t.Fatalf("%d of %d segments held, want the bound to split them", inMemory, len(held))
	}
	q.FlushPending()
	if !reflect.DeepEqual(batches, want) {
		t.Fatal("flushed records differ from the files")
	}
	if q.retainedBytes != 0 {
		t.Fatalf("%d bytes still held after the flush", q.retainedBytes)
	}
}

// DecodedTime gives what a JSON round trip of the time gives.
func TestDecodedTimeMatchesJSON(t *testing.T) {
	now := time.Now()
	for _, tt := range []time.Time{
		{},
		now,
		now.UTC(),
		now.In(time.FixedZone("NAMED", 3600)),
		now.In(time.FixedZone("", 5*3600+30*60)),
		now.In(time.FixedZone("LMT", 4*3600+37*60+11)),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	} {
		b, err := json.Marshal(tt)
		if err != nil {
			t.Fatal(err)
		}
		var want time.Time
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		got, ok := DecodedTime(tt)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("DecodedTime(%v) = %#v, %v; want %#v", tt, got, ok, want)
		}
	}
	if _, ok := DecodedTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("DecodedTime accepted a year JSON cannot encode")
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = DecodedTime(now) }); n != 0 {
		t.Fatalf("DecodedTime allocates %v times", n)
	}
}

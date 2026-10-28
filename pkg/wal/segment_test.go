package wal

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/segments.golden from this run")

func goldenRecords() []testRecord {
	return []testRecord{
		{ID: "plain", N: 1},
		{},
		{ID: "html", N: -7, Notes: `<a href="x?a=1&b=2">link</a>`},
		{ID: "unicode", N: 3, Notes: "héllo 世界 \u2028\u2029 😀"},
		{ID: "control", N: 4, Notes: "tab\tnl\ncr\rnul\x00bell\x07"},
		{ID: "quotes", N: 6, Notes: `"quoted" \backslash\ /slash`},
		{ID: "tags", N: 7, Tags: map[string]string{"z": "last", "a": "first", "m": "<mid>"}},
		{ID: "long", N: 8, Notes: strings.Repeat("0123456789", 50)},
	}
}

// The segment files hold exactly what earlier releases wrote, so replayed and
// already-shipped segments decode the same.
func TestSegmentBytesMatchGolden(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 4}, noFlush[testRecord])
	recs := goldenRecords()
	for _, r := range recs[:3] {
		if err := q.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.WriteBatch(recs[3:]); err != nil {
		t.Fatal(err)
	}
	q.StopBackground()

	got := strings.Join(walFiles(t, dir), "--- next file\n")
	golden := filepath.Join("testdata", "segments.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("segment bytes differ from %s:\n got %q\nwant %q", golden, got, want)
	}
}

// Each line is json.Marshal plus a newline, also for records larger than the
// write buffer and for small records written after them.
func TestSegmentBytesMatchMarshal(t *testing.T) {
	var recs []testRecord
	for i, size := range []int{10, 3000, writeBufferSize - 40, writeBufferSize + 1, 5, 3 * writeBufferSize, 1, 200} {
		recs = append(recs, testRecord{ID: fmt.Sprintf("r%d", i), N: i, Notes: strings.Repeat(string(rune('a'+i)), size)})
	}
	// The JSON engines differ on invalid UTF-8, so it is pinned here, not in the golden.
	recs = append(recs, testRecord{ID: "invalid-utf8", Notes: "a\xffb\xc3"})
	recs = append(recs, testRecords(50)...)

	var want strings.Builder
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		want.Write(b)
		want.WriteByte('\n')
	}

	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			dir := t.TempDir()
			q := openTestQueue(t, Config{Dir: dir, MaxLines: 1000}, noFlush[testRecord])
			if batch {
				if err := q.WriteBatch(recs); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, r := range recs {
					if err := q.Write(r); err != nil {
						t.Fatal(err)
					}
				}
			}
			q.StopBackground()
			got, err := os.ReadFile(filepath.Join(dir, ActiveName))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want.String() {
				t.Fatalf("active segment differs from json.Marshal output (%d vs %d bytes)", len(got), want.Len())
			}
		})
	}
}

// lineRecord returns a record whose JSON line, newline included, is n bytes.
func lineRecord(t *testing.T, id string, n int) testRecord {
	t.Helper()
	r := testRecord{ID: id, Notes: "x"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Notes = strings.Repeat("x", n-len(b))
	return r
}

// Lines around and past the reader's buffer size come back intact, and
// records sharing one buffer fill do not overwrite each other.
func TestRecoverReadsEveryRecord(t *testing.T) {
	const readerBuf = 4096
	var recs []testRecord
	for i, n := range []int{readerBuf - 1, readerBuf, readerBuf + 1, 2*readerBuf + 3, 3 * readerBuf, 100_000} {
		recs = append(recs, testRecords(7)...)
		recs = append(recs, lineRecord(t, fmt.Sprintf("long%d", i), n))
	}
	recs = append(recs, testRecords(300)...)

	dir := t.TempDir()
	writer := openTestQueue(t, Config{Dir: dir, MaxLines: 100}, noFlush[testRecord])
	if err := writer.WriteBatch(recs); err != nil {
		t.Fatal(err)
	}
	writer.StopBackground()

	var got []testRecord
	q := openTestQueue(t, Config{Dir: dir, MaxLines: 100}, func(batch []testRecord) error {
		got = append(got, batch...)
		return nil
	})
	defer q.StopBackground()
	q.Recover()

	if len(got) != len(recs) {
		t.Fatalf("recovered %d records, want %d", len(got), len(recs))
	}
	for i := range recs {
		if !reflect.DeepEqual(got[i], recs[i]) {
			t.Fatalf("record %d = %+v, want %+v", i, got[i], recs[i])
		}
	}
}

func TestReadSegmentLineEndings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segment-1.jsonl")
	body := "{\"id\":\"a\",\"n\":1}\r\n\n{bad\n" + `{"id":"` + strings.Repeat("b", 5000) + "\"}\r\n" + `{"id":"torn-but-whole","n":2}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSegment[testRecord](path, 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	want := []testRecord{{ID: "a", N: 1}, {ID: strings.Repeat("b", 5000)}, {ID: "torn-but-whole", N: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readSegment = %+v, want %+v", got, want)
	}
}

// A segment rotated by this process is read into a slice sized once, and its
// recorded line count is forgotten after the flush.
func TestFlushSizesRecordsFromRotation(t *testing.T) {
	var batches [][]testRecord
	q := openTestQueue(t, Config{MaxLines: 4}, func(recs []testRecord) error {
		batches = append(batches, recs)
		return nil
	})
	defer q.StopBackground()
	if err := q.WriteBatch(testRecords(10)); err != nil {
		t.Fatal(err)
	}
	q.Recover()

	if len(batches) != 3 {
		t.Fatalf("flushed %d segments, want 3", len(batches))
	}
	for i, recs := range batches {
		if cap(recs) != len(recs) {
			t.Fatalf("segment %d: cap %d, len %d", i, cap(recs), len(recs))
		}
	}
	if n := len(q.segmentLines); n != 0 {
		t.Fatalf("%d segment line counts left after the flush", n)
	}
}

// BenchmarkQueueWriteFlush writes 10k records in 512-record batches, rotates them
// into one segment and flushes it, so it covers the write and the read-back.
func BenchmarkQueueWriteFlush(b *testing.B) {
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
	flushed := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		q := openTestQueue(b, Config{MaxLines: records}, func(recs []testRecord) error {
			flushed += len(recs)
			return nil
		})
		b.StartTimer()
		for start := 0; start < records; start += 512 {
			if err := q.WriteBatch(recs[start:min(start+512, records)]); err != nil {
				b.Fatal(err)
			}
		}
		q.FlushPending()
		b.StopTimer()
		q.StopBackground()
		b.StartTimer()
	}
	if flushed != b.N*records {
		b.Fatalf("flushed %d records, want %d", flushed, b.N*records)
	}
}

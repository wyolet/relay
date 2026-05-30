package payload

import (
	"testing"
	"time"
)

func rec(id string, ts time.Time, status int, mut func(*Record)) Record {
	r := Record{RequestID: id, Timestamp: ts, Status: status, Source: "pipeline"}
	if mut != nil {
		mut(&r)
	}
	return r
}

func TestFilterRecords_Dimensions(t *testing.T) {
	now := time.Now().UTC()
	recs := []Record{
		rec("a", now, 200, func(r *Record) { r.PolicyID = "p1"; r.ModelID = "m1" }),
		rec("b", now, 500, func(r *Record) { r.PolicyID = "p2"; r.ErrorKind = "upstream_error" }),
		rec("c", now, 200, func(r *Record) { r.PolicyID = "p1"; r.ModelID = "m2" }),
	}

	got := FilterRecords(recs, Query{PolicyID: []string{"p1"}})
	if len(got) != 2 {
		t.Fatalf("policy filter: want 2, got %d", len(got))
	}

	got = FilterRecords(recs, Query{StatusMin: 400})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("status filter: want [b], got %+v", got)
	}

	got = FilterRecords(recs, Query{ModelID: []string{"m1", "m2"}})
	if len(got) != 2 {
		t.Fatalf("model set filter: want 2, got %d", len(got))
	}

	got = FilterRecords(recs, Query{ErrorKind: []string{"upstream_error"}})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("error_kind filter: want [b], got %+v", got)
	}
}

func TestFilterRecords_TimeWindow(t *testing.T) {
	now := time.Now().UTC()
	recs := []Record{
		rec("old", now.Add(-48*time.Hour), 200, nil),
		rec("new", now.Add(-1*time.Minute), 200, nil),
	}
	got := FilterRecords(recs, Query{Since: time.Hour})
	if len(got) != 1 || got[0].RequestID != "new" {
		t.Fatalf("since window: want [new], got %+v", got)
	}

	got = FilterRecords(recs, Query{From: now.Add(-24 * time.Hour), To: now})
	if len(got) != 1 || got[0].RequestID != "new" {
		t.Fatalf("from/to window: want [new], got %+v", got)
	}
}

func TestFilterRecords_Cursor(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	recs := []Record{
		rec("a", base.Add(2*time.Second), 200, nil),
		rec("b", base.Add(1*time.Second), 200, nil),
		rec("c", base, 200, nil),
	}
	// Cursor at b → only records strictly older than (b.ts, "b").
	got := SortAndLimit(FilterRecords(recs, Query{CursorTS: base.Add(1 * time.Second), CursorID: "b"}), 10)
	if len(got) != 1 || got[0].RequestID != "c" {
		t.Fatalf("cursor: want [c], got %+v", got)
	}
}

func TestSortAndLimit_NewestFirstAndCap(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	recs := []Record{
		rec("c", base, 200, nil),
		rec("a", base.Add(2*time.Second), 200, nil),
		rec("b", base.Add(1*time.Second), 200, nil),
	}
	got := SortAndLimit(recs, 2)
	if len(got) != 2 || got[0].RequestID != "a" || got[1].RequestID != "b" {
		t.Fatalf("sort+limit: want [a b], got %+v", got)
	}
}

func TestStripBody(t *testing.T) {
	r := Record{RequestID: "a", RequestBody: []byte("x"), ResponseBody: []byte("y"), RequestTruncated: true}
	got := StripBody(r)
	if got.RequestBody != nil || got.ResponseBody != nil {
		t.Fatalf("bodies not stripped: %+v", got)
	}
	if !got.RequestTruncated || got.RequestID != "a" {
		t.Fatalf("metadata lost: %+v", got)
	}
}

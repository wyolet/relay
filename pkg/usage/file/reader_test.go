package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// writeFixture writes a few representative events to a tmp file and
// returns the path.
func writeFixture(t *testing.T, events []usage.Event) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	sink, err := NewSink(path)
	if err != nil {
		t.Fatalf("file sink: %v", err)
	}
	for _, ev := range events {
		if err := sink.Write(ev); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return path
}

func TestReader_Events_FiltersAndLimit(t *testing.T) {
	now := time.Now()
	path := writeFixture(t, []usage.Event{
		{RequestID: "a", Source: "pipeline", Timestamp: now.Add(-5 * time.Minute), Status: 200, ModelID: "m1"},
		{RequestID: "b", Source: "proxy", Timestamp: now.Add(-2 * time.Minute), Status: 500, ModelID: "m1"},
		{RequestID: "c", Source: "pipeline", Timestamp: now.Add(-1 * time.Minute), Status: 200, ModelID: "m2"},
	})
	r := NewReader(path)

	// All within last hour, no filter
	got, err := r.Events(context.Background(), usage.EventQuery{Since: time.Hour})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events got %d", len(got))
	}
	// Newest-first
	if got[0].RequestID != "c" || got[2].RequestID != "a" {
		t.Fatalf("not sorted newest-first: %v", []string{got[0].RequestID, got[1].RequestID, got[2].RequestID})
	}

	// Source filter
	got, _ = r.Events(context.Background(), usage.EventQuery{Since: time.Hour, Source: []string{"pipeline"}})
	if len(got) != 2 {
		t.Fatalf("source pipeline: want 2 got %d", len(got))
	}

	// Status range filter — errors only
	got, _ = r.Events(context.Background(), usage.EventQuery{Since: time.Hour, StatusMin: 400})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("status>=400: want [b] got %+v", got)
	}

	// Limit
	got, _ = r.Events(context.Background(), usage.EventQuery{Since: time.Hour, Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit=2: want 2 got %d", len(got))
	}
}

func TestReader_Summary_GroupAggregation(t *testing.T) {
	now := time.Now()
	path := writeFixture(t, []usage.Event{
		{Timestamp: now, Source: "pipeline", Status: 200, ModelID: "m1", DurationMs: 100, Tokens: sdkusage.Tokens{"input": 10, "output": 5}},
		{Timestamp: now, Source: "pipeline", Status: 200, ModelID: "m1", DurationMs: 200, Tokens: sdkusage.Tokens{"input": 20, "output": 10}},
		{Timestamp: now, Source: "pipeline", Status: 500, ModelID: "m2", DurationMs: 50, Tokens: nil},
	})
	r := NewReader(path)

	res, err := r.Summary(context.Background(), usage.SummaryQuery{
		EventQuery: usage.EventQuery{Since: time.Hour},
		GroupBy:    "model_id",
	})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("want 2 groups got %d", len(res.Rows))
	}

	// Find m1 row (should be first — more requests)
	if res.Rows[0].Group["model_id"] != "m1" {
		t.Fatalf("expected m1 first, got %+v", res.Rows[0].Group)
	}
	if res.Rows[0].Requests != 2 {
		t.Fatalf("m1 requests: got %d", res.Rows[0].Requests)
	}
	if res.Rows[0].Tokens["input"] != 30 || res.Rows[0].Tokens["output"] != 15 {
		t.Fatalf("m1 tokens: %+v", res.Rows[0].Tokens)
	}
	if res.Rows[0].DurationMs.Avg != 150 {
		t.Fatalf("m1 avg duration: %d", res.Rows[0].DurationMs.Avg)
	}

	// m2 row
	if res.Rows[1].Group["model_id"] != "m2" || res.Rows[1].ErrorCount != 1 {
		t.Fatalf("m2 row wrong: %+v", res.Rows[1])
	}
}

func TestReader_Events_RichFilters(t *testing.T) {
	now := time.Now()
	path := writeFixture(t, []usage.Event{
		{RequestID: "a", Timestamp: now.Add(-10 * time.Minute), Status: 200, ModelID: "m1", Source: "pipeline", FinishReason: "stop"},
		{RequestID: "b", Timestamp: now.Add(-5 * time.Minute), Status: 500, ModelID: "m2", Source: "proxy", ErrorKind: "upstream_5xx"},
		{RequestID: "c", Timestamp: now.Add(-1 * time.Minute), Status: 200, ModelID: "m3", Source: "pipeline", FinishReason: "length"},
	})
	r := NewReader(path)
	ctx := context.Background()

	// Multi-value model_id: m1 OR m3.
	got, _ := r.Events(ctx, usage.EventQuery{Since: time.Hour, ModelID: []string{"m1", "m3"}})
	if len(got) != 2 {
		t.Fatalf("multi-value model_id: want 2, got %d", len(got))
	}

	// request_id exact lookup.
	got, _ = r.Events(ctx, usage.EventQuery{RequestID: "b"})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("request_id lookup: %+v", got)
	}

	// finish_reason filter.
	got, _ = r.Events(ctx, usage.EventQuery{Since: time.Hour, FinishReason: []string{"length"}})
	if len(got) != 1 || got[0].RequestID != "c" {
		t.Fatalf("finish_reason filter: %+v", got)
	}

	// error_kind filter.
	got, _ = r.Events(ctx, usage.EventQuery{Since: time.Hour, ErrorKind: []string{"upstream_5xx"}})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("error_kind filter: %+v", got)
	}

	// Absolute from/to window: only the middle event.
	got, _ = r.Events(ctx, usage.EventQuery{
		From: now.Add(-7 * time.Minute),
		To:   now.Add(-3 * time.Minute),
	})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("from/to window: %+v", got)
	}
}

func TestReader_Events_KeysetPagination(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	// Two events share a timestamp (tie) to exercise the request_id tiebreak.
	tie := now.Add(-3 * time.Minute)
	path := writeFixture(t, []usage.Event{
		{RequestID: "e", Timestamp: now.Add(-1 * time.Minute)},
		{RequestID: "d", Timestamp: now.Add(-2 * time.Minute)},
		{RequestID: "c2", Timestamp: tie},
		{RequestID: "c1", Timestamp: tie},
		{RequestID: "a", Timestamp: now.Add(-4 * time.Minute)},
	})
	r := NewReader(path)
	ctx := context.Background()

	// Walk pages of 2, accumulating request_ids; expect newest-first with no
	// gaps or repeats. Tie bucket orders by request_id DESC: c2 before c1.
	want := []string{"e", "d", "c2", "c1", "a"}
	var got []string
	q := usage.EventQuery{Limit: 2}
	for {
		page, err := r.Events(ctx, q)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			got = append(got, ev.RequestID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		q.CursorTS, q.CursorID = last.Timestamp, last.RequestID
	}
	if len(got) != len(want) {
		t.Fatalf("paged %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page order: got %v want %v", got, want)
		}
	}
}

func TestReader_TimeSeries_Bucketing(t *testing.T) {
	// Fixed epoch-aligned base so bucket boundaries are deterministic.
	base := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	path := writeFixture(t, []usage.Event{
		{Timestamp: base, Source: "pipeline", Status: 200, ModelID: "m1", Tokens: sdkusage.Tokens{"input": 10}},
		{Timestamp: base.Add(5 * time.Minute), Source: "pipeline", Status: 200, ModelID: "m1", Tokens: sdkusage.Tokens{"input": 20}},
		{Timestamp: base.Add(2 * time.Hour), Source: "pipeline", Status: 500, ModelID: "m1"},
		{Timestamp: base.Add(3 * time.Minute), Source: "proxy", Status: 200, ModelID: "m2", Tokens: sdkusage.Tokens{"input": 5}},
	})
	r := NewReader(path)
	all := usage.EventQuery{} // Since:0 → no lower bound

	// Single series, 1h buckets: bucket0 has 3 events, bucket+2h has 1.
	ts, err := r.TimeSeries(context.Background(), usage.TimeSeriesQuery{EventQuery: all, Interval: time.Hour})
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}
	if ts.Interval != time.Hour.String() {
		t.Fatalf("interval echo: %q", ts.Interval)
	}
	if len(ts.Rows) != 1 || ts.Rows[0].Group != nil {
		t.Fatalf("want single ungrouped series, got %+v", ts.Rows)
	}
	pts := ts.Rows[0].Points
	if len(pts) != 2 {
		t.Fatalf("want 2 buckets, got %d: %+v", len(pts), pts)
	}
	if !pts[0].Bucket.Before(pts[1].Bucket) {
		t.Fatalf("points not oldest-first: %v %v", pts[0].Bucket, pts[1].Bucket)
	}
	if pts[0].Requests != 3 || pts[0].Tokens["input"] != 35 {
		t.Fatalf("bucket0: reqs=%d tokens=%+v", pts[0].Requests, pts[0].Tokens)
	}
	if pts[1].Requests != 1 || pts[1].ErrorCount != 1 {
		t.Fatalf("bucket1: reqs=%d errs=%d", pts[1].Requests, pts[1].ErrorCount)
	}

	// Grouped by model_id: m1 (3 reqs, 2 buckets) sorts before m2 (1 req).
	tsg, err := r.TimeSeries(context.Background(), usage.TimeSeriesQuery{EventQuery: all, Interval: time.Hour, GroupBy: "model_id"})
	if err != nil {
		t.Fatalf("timeseries grouped: %v", err)
	}
	if len(tsg.Rows) != 2 {
		t.Fatalf("want 2 series, got %d", len(tsg.Rows))
	}
	if tsg.Rows[0].Group["model_id"] != "m1" || len(tsg.Rows[0].Points) != 2 {
		t.Fatalf("m1 series wrong: %+v", tsg.Rows[0])
	}
	if tsg.Rows[1].Group["model_id"] != "m2" || len(tsg.Rows[1].Points) != 1 {
		t.Fatalf("m2 series wrong: %+v", tsg.Rows[1])
	}
}

func TestReader_TimeSeries_Invalid(t *testing.T) {
	r := NewReader(writeFixture(t, nil))
	if _, err := r.TimeSeries(context.Background(), usage.TimeSeriesQuery{Interval: 0}); err == nil {
		t.Fatal("expected error for zero interval")
	}
	if _, err := r.TimeSeries(context.Background(), usage.TimeSeriesQuery{Interval: time.Hour, GroupBy: "bogus"}); err == nil {
		t.Fatal("expected error for invalid group_by")
	}
}

func TestReader_Summary_InvalidGroupBy(t *testing.T) {
	path := writeFixture(t, nil)
	r := NewReader(path)
	_, err := r.Summary(context.Background(), usage.SummaryQuery{GroupBy: "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid group_by")
	}
}

func TestReader_MissingFile(t *testing.T) {
	// Non-existent path → empty result, not an error. Useful for boot
	// before any request has fired.
	r := NewReader("/nonexistent/usage.jsonl")
	got, err := r.Events(context.Background(), usage.EventQuery{Since: time.Hour})
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

func TestReader_MalformedLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	content := []byte(`{"request_id":"good","source":"pipeline","ts":"` + time.Now().Format(time.RFC3339) + `","status":200}
this is not json
{"request_id":"alsogood","source":"proxy","ts":"` + time.Now().Format(time.RFC3339) + `","status":200}
`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := NewReader(path)
	got, err := r.Events(context.Background(), usage.EventQuery{Since: time.Hour})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 valid events got %d", len(got))
	}
}

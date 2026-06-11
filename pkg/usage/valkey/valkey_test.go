package valkey_test

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/usage/valkey"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

func newSink(t *testing.T) *valkey.Sink {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	return valkey.New(mem, valkey.Config{TTL: time.Minute})
}

func event(requestID string, ts time.Time, status int, source string) usage.Event {
	return usage.Event{
		RequestID:  requestID,
		Timestamp:  ts,
		Status:     status,
		Source:     source,
		DurationMs: 10,
	}
}

func TestWriteThenEvents(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	now := time.Now()
	ev := event("req-1", now, 200, "pipeline")
	ev.Model, ev.Host, ev.Policy = "gpt-4o", "openai", "default"
	ev.Provider, ev.Pricing = "openai", "openai-gpt-4o"
	cost := int64(7_500_000)
	ev.CostNanos = &cost
	ev.CostBreakdown = map[string]int64{"tokens.input": 7_500_000}

	if err := sk.Write(ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := sk.Events(ctx, usage.EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].RequestID != "req-1" {
		t.Errorf("wrong request_id: %s", got[0].RequestID)
	}
	if got[0].Model != "gpt-4o" || got[0].Host != "openai" || got[0].Policy != "default" {
		t.Errorf("slug round-trip: %+v", got[0])
	}
	if got[0].Provider != "openai" || got[0].Pricing != "openai-gpt-4o" {
		t.Errorf("provider/pricing round-trip: %+v", got[0])
	}
	if got[0].CostNanos == nil || *got[0].CostNanos != 7_500_000 ||
		got[0].CostBreakdown["tokens.input"] != 7_500_000 {
		t.Errorf("cost round-trip: %+v", got[0])
	}
}

func TestEventsNewestFirstAndLimit(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	base := time.Now()

	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		if err := sk.Write(event("req-"+string(rune('A'+i)), ts, 200, "pipeline")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	got, err := sk.Events(ctx, usage.EventQuery{Limit: 3})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// Newest first: req-E (i=4) should be first.
	if got[0].RequestID != "req-E" {
		t.Errorf("want req-E first, got %s", got[0].RequestID)
	}
	if !got[0].Timestamp.After(got[1].Timestamp) {
		t.Error("events not in descending time order")
	}
}

func TestTimeSeries(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)

	evs := []usage.Event{
		{RequestID: "a", Timestamp: base, Status: 200, ModelID: "m1", Tokens: sdkusage.Tokens{"input": 10}},
		{RequestID: "b", Timestamp: base.Add(5 * time.Minute), Status: 200, ModelID: "m1", Tokens: sdkusage.Tokens{"input": 20}},
		{RequestID: "c", Timestamp: base.Add(2 * time.Hour), Status: 500, ModelID: "m1"},
		{RequestID: "d", Timestamp: base.Add(time.Minute), Status: 200, ModelID: "m2", Tokens: sdkusage.Tokens{"input": 5}},
	}
	for _, ev := range evs {
		if err := sk.Write(ev); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	ts, err := sk.TimeSeries(ctx, usage.TimeSeriesQuery{Interval: time.Hour})
	if err != nil {
		t.Fatalf("TimeSeries: %v", err)
	}
	if len(ts.Rows) != 1 {
		t.Fatalf("want single series, got %d rows", len(ts.Rows))
	}
	pts := ts.Rows[0].Points
	if len(pts) != 2 || pts[0].Requests != 3 || pts[0].Tokens["input"] != 35 || pts[1].ErrorCount != 1 {
		t.Fatalf("unexpected buckets: %+v", pts)
	}

	tsg, err := sk.TimeSeries(ctx, usage.TimeSeriesQuery{Interval: time.Hour, GroupBy: "model_id"})
	if err != nil {
		t.Fatalf("TimeSeries grouped: %v", err)
	}
	if len(tsg.Rows) != 2 || tsg.Rows[0].Group["model_id"] != "m1" {
		t.Fatalf("grouped series wrong: %+v", tsg.Rows)
	}
}

func TestDimensionFilter(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	now := time.Now()

	if err := sk.Write(event("r1", now, 200, "pipeline")); err != nil {
		t.Fatal(err)
	}
	ev2 := event("r2", now.Add(time.Millisecond), 200, "proxy")
	if err := sk.Write(ev2); err != nil {
		t.Fatal(err)
	}

	got, err := sk.Events(ctx, usage.EventQuery{Source: []string{"proxy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "r2" {
		t.Errorf("source filter: want [r2], got %v", got)
	}
}

func TestStatusFilter(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	now := time.Now()

	_ = sk.Write(event("ok", now, 200, "pipeline"))
	_ = sk.Write(event("err", now.Add(time.Millisecond), 500, "pipeline"))

	got, err := sk.Events(ctx, usage.EventQuery{StatusMin: 400})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "err" {
		t.Errorf("status filter: want [err], got %v", got)
	}
}

func TestSummaryGrouping(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	now := time.Now()

	_ = sk.Write(event("r1", now, 200, "pipeline"))
	_ = sk.Write(event("r2", now.Add(time.Millisecond), 200, "pipeline"))
	_ = sk.Write(event("r3", now.Add(2*time.Millisecond), 500, "proxy"))

	res, err := sk.Summary(ctx, usage.SummaryQuery{GroupBy: "source"})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(res.Rows))
	}
	// First row should be pipeline (2 requests > 1).
	if res.Rows[0].Group["source"] != "pipeline" {
		t.Errorf("want pipeline first, got %v", res.Rows[0].Group)
	}
	if res.Rows[0].Requests != 2 {
		t.Errorf("pipeline: want 2 requests, got %d", res.Rows[0].Requests)
	}
}

func TestMalformedValueSkipped(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	ctx := context.Background()

	// Inject a bad value directly at the expected key prefix.
	_ = mem.Set(ctx, "usagevk:{u}:00000000000000000001:bad", []byte("not-json"), time.Minute)

	sk := valkey.New(mem, valkey.Config{TTL: time.Minute})
	_ = sk.Write(usage.Event{RequestID: "good", Timestamp: time.Now(), Status: 200, Source: "pipeline"})

	got, err := sk.Events(ctx, usage.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "good" {
		t.Errorf("want only good event, got %v", got)
	}
}

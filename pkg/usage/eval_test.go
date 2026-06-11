package usage

import (
	"strings"
	"testing"
	"time"

	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// Pre-upstream rejections (LogOnly: status 0 + error_kind) must not reach
// usage aggregates — they belong to the logs view. Upstream errors
// (status >= 400) stay, counted in ErrorCount.
func TestSummarize_ExcludesLogOnly(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{RequestID: "ok", ModelID: "m1", Timestamp: now, Status: 200,
			Tokens: sdkusage.Tokens{"input": 10, "output": 5}},
		{RequestID: "upstream-err", ModelID: "m1", Timestamp: now, Status: 500,
			ErrorKind: "upstream_5xx"},
		{RequestID: "rejected", RequestedModel: "m1", Timestamp: now, Status: 0,
			ErrorKind: "model_not_found"},
		{RequestID: "no-keys", ModelID: "m1", Timestamp: now, Status: 0,
			ErrorKind: "no_keys"},
	}

	res, err := Summarize(events, "model_id")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 group (LogOnly rows excluded, no empty-model bucket), got %d: %+v", len(res.Rows), res.Rows)
	}
	row := res.Rows[0]
	if row.Group["model_id"] != "m1" {
		t.Fatalf("group: %+v", row.Group)
	}
	if row.Requests != 2 {
		t.Fatalf("requests: want 2 (ok + upstream error), got %d", row.Requests)
	}
	if row.ErrorCount != 1 {
		t.Fatalf("errors: want 1 (the 500), got %d", row.ErrorCount)
	}
	if row.Tokens["input"] != 10 || row.Tokens["output"] != 5 {
		t.Fatalf("tokens: %+v", row.Tokens)
	}

	ts, err := Bucketize(events, time.Hour, "")
	if err != nil {
		t.Fatalf("Bucketize: %v", err)
	}
	if len(ts.Rows) != 1 || len(ts.Rows[0].Points) != 1 {
		t.Fatalf("timeseries shape: %+v", ts.Rows)
	}
	p := ts.Rows[0].Points[0]
	if p.Requests != 2 || p.ErrorCount != 1 {
		t.Fatalf("timeseries point: want reqs=2 errs=1, got reqs=%d errs=%d", p.Requests, p.ErrorCount)
	}
}

func TestAggregates_LatencyTTFTAndErrorSplit(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{RequestID: "a", ModelID: "m1", Timestamp: now, Status: 200, DurationMs: 100,
			FinishReason: "stop", Upstream: &sdkusage.UpstreamTiming{ResponseStart: 50_000}},
		{RequestID: "b", ModelID: "m1", Timestamp: now, Status: 200, DurationMs: 300,
			FinishReason: "length", Upstream: &sdkusage.UpstreamTiming{ResponseStart: 150_000}},
		{RequestID: "c", ModelID: "m1", Timestamp: now, Status: 429, DurationMs: 20,
			ErrorKind: "upstream_429"},
		{RequestID: "d", ModelID: "m1", Timestamp: now, Status: 502, DurationMs: 40,
			ErrorKind: "upstream_5xx"},
	}

	res, err := Summarize(events, "model_id")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	row := res.Rows[0]
	if row.TTFTMs == nil {
		t.Fatal("ttft_ms: want stats over the 2 events with upstream timing, got nil")
	}
	if row.TTFTMs.Max != 150 || row.TTFTMs.P50 != 50 {
		t.Fatalf("ttft_ms: %+v", *row.TTFTMs)
	}
	if row.DurationMs.Max != 300 {
		t.Fatalf("duration_ms: %+v", row.DurationMs)
	}

	// New group dimensions.
	byFinish, err := Summarize(events, "finish_reason")
	if err != nil {
		t.Fatalf("Summarize finish_reason: %v", err)
	}
	if len(byFinish.Rows) != 3 { // "stop", "length", "" (the two errors)
		t.Fatalf("finish_reason groups: %+v", byFinish.Rows)
	}
	byErrKind, err := Summarize(events, "error_kind")
	if err != nil {
		t.Fatalf("Summarize error_kind: %v", err)
	}
	if len(byErrKind.Rows) != 3 { // "", "upstream_429", "upstream_5xx"
		t.Fatalf("error_kind groups: %+v", byErrKind.Rows)
	}

	ts, err := Bucketize(events, time.Hour, "")
	if err != nil {
		t.Fatalf("Bucketize: %v", err)
	}
	p := ts.Rows[0].Points[0]
	if p.ErrorCount != 2 || p.Errors4xx != 1 || p.Errors5xx != 1 {
		t.Fatalf("error split: errs=%d 4xx=%d 5xx=%d", p.ErrorCount, p.Errors4xx, p.Errors5xx)
	}
	if p.DurationMs.Max != 300 {
		t.Fatalf("bucket duration_ms: %+v", p.DurationMs)
	}
	if p.TTFTMs == nil || p.TTFTMs.Max != 150 {
		t.Fatalf("bucket ttft_ms: %+v", p.TTFTMs)
	}

	// A bucket with no upstream timing omits ttft entirely.
	noTTFT, err := Bucketize(events[2:], time.Hour, "")
	if err != nil {
		t.Fatalf("Bucketize no-ttft: %v", err)
	}
	if noTTFT.Rows[0].Points[0].TTFTMs != nil {
		t.Fatalf("want nil ttft_ms, got %+v", noTTFT.Rows[0].Points[0].TTFTMs)
	}
}

func TestTags_FilterAndGroupBy(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{RequestID: "a", Source: "pipeline", Timestamp: now, Status: 200,
			Tags: map[string]string{"session_id": "s1", "leg": "bytepass"}},
		{RequestID: "b", Source: "pipeline", Timestamp: now, Status: 200,
			Tags: map[string]string{"session_id": "s1", "leg": "translate"}},
		{RequestID: "c", Source: "pipeline", Timestamp: now, Status: 200,
			Tags: map[string]string{"session_id": "s2"}},
		{RequestID: "d", Source: "pipeline", Timestamp: now, Status: 200}, // untagged
	}

	got := FilterEvents(events, EventQuery{Tags: map[string][]string{"session_id": {"s1"}}})
	if len(got) != 2 {
		t.Fatalf("single value: want 2, got %d", len(got))
	}
	got = FilterEvents(events, EventQuery{Tags: map[string][]string{"session_id": {"s1", "s2"}}})
	if len(got) != 3 {
		t.Fatalf("OR within key: want 3, got %d", len(got))
	}
	got = FilterEvents(events, EventQuery{Tags: map[string][]string{"session_id": {"s1"}, "leg": {"translate"}}})
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("AND across keys: %+v", got)
	}
	// A missing key matches only an explicit "" value.
	got = FilterEvents(events, EventQuery{Tags: map[string][]string{"leg": {""}}})
	if len(got) != 2 {
		t.Fatalf("missing-key as empty: want 2 (c + d), got %d", len(got))
	}

	res, err := Summarize(events, "tags.session_id")
	if err != nil {
		t.Fatalf("Summarize tags.session_id: %v", err)
	}
	if len(res.Rows) != 3 { // s1(2), s2(1), ""(1)
		t.Fatalf("tag groups: %+v", res.Rows)
	}
	if res.Rows[0].Group["tags.session_id"] != "s1" || res.Rows[0].Requests != 2 {
		t.Fatalf("top tag group: %+v", res.Rows[0])
	}

	ts, err := Bucketize(events, time.Hour, "tags.leg")
	if err != nil {
		t.Fatalf("Bucketize tags.leg: %v", err)
	}
	if len(ts.Rows) != 3 { // "bytepass", "translate", ""
		t.Fatalf("tag series: %+v", ts.Rows)
	}

	if _, err := Summarize(events, "tags."); err == nil {
		t.Fatal("empty tag key: want error")
	}
}

func TestTagGroupKey(t *testing.T) {
	if key, ok := TagGroupKey("tags.session_id"); !ok || key != "session_id" {
		t.Fatalf("tags.session_id: got %q, %v", key, ok)
	}
	for _, bad := range []string{"tags.", "tags", "session_id", "tags." + strings.Repeat("k", MaxTagKeyLen+1)} {
		if _, ok := TagGroupKey(bad); ok {
			t.Fatalf("want invalid: %q", bad)
		}
	}
	if !IsValidGroupBy("tags.leg") {
		t.Fatal("IsValidGroupBy(tags.leg): want true")
	}
	if IsValidGroupBy("tags.") {
		t.Fatal("IsValidGroupBy(tags.): want false")
	}
}

func TestSlugDimensions_FilterAndGroupBy(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{RequestID: "a", Timestamp: now, Status: 200,
			Model: "gpt-4o", Host: "openai", Policy: "default"},
		{RequestID: "b", Timestamp: now, Status: 200,
			Model: "claude-sonnet", Host: "anthropic", Policy: "default"},
		{RequestID: "c", Timestamp: now, Status: 500,
			Model: "gpt-4o", Host: "azure", Policy: "premium"},
	}

	for _, g := range []string{"model", "host", "policy"} {
		if !IsValidGroupBy(g) {
			t.Fatalf("IsValidGroupBy(%q) = false", g)
		}
	}

	byModel, err := Summarize(events, "model")
	if err != nil {
		t.Fatalf("Summarize model: %v", err)
	}
	if len(byModel.Rows) != 2 {
		t.Fatalf("model groups: want 2, got %+v", byModel.Rows)
	}
	if byModel.Rows[0].Group["model"] != "gpt-4o" || byModel.Rows[0].Requests != 2 {
		t.Fatalf("top model group: %+v", byModel.Rows[0])
	}

	byHost, err := Summarize(events, "host")
	if err != nil {
		t.Fatalf("Summarize host: %v", err)
	}
	if len(byHost.Rows) != 3 {
		t.Fatalf("host groups: want 3, got %+v", byHost.Rows)
	}

	byPolicy, err := Summarize(events, "policy")
	if err != nil {
		t.Fatalf("Summarize policy: %v", err)
	}
	if len(byPolicy.Rows) != 2 || byPolicy.Rows[0].Group["policy"] != "default" {
		t.Fatalf("policy groups: %+v", byPolicy.Rows)
	}

	ts, err := Bucketize(events, time.Hour, "model")
	if err != nil {
		t.Fatalf("Bucketize model: %v", err)
	}
	if len(ts.Rows) != 2 || ts.Rows[0].Group["model"] != "gpt-4o" {
		t.Fatalf("timeseries model groups: %+v", ts.Rows)
	}

	got := FilterEvents(events, EventQuery{Model: []string{"gpt-4o"}})
	if len(got) != 2 {
		t.Fatalf("model filter: want 2, got %d", len(got))
	}
	got = FilterEvents(events, EventQuery{Host: []string{"azure", "anthropic"}})
	if len(got) != 2 {
		t.Fatalf("host filter: want 2, got %d", len(got))
	}
	got = FilterEvents(events, EventQuery{Policy: []string{"premium"}})
	if len(got) != 1 || got[0].RequestID != "c" {
		t.Fatalf("policy filter: %+v", got)
	}
}

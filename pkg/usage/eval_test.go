package usage

import (
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

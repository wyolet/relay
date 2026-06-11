package usage

import (
	"encoding/json"
	"testing"
	"time"

	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// The JSONL contract: field order is fixed (Go marshals in struct order)
// and new fields are append-only — JSONL / jq / column-mapped backends
// rely on it. A failure here means a field was inserted mid-struct or a
// json key was renamed; both break stored data.
func TestEvent_MarshalStability(t *testing.T) {
	ev := Event{
		RequestID:      "r1",
		Source:         "pipeline",
		Timestamp:      time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		Status:         200,
		DurationMs:     42,
		Streamed:       true,
		FinishReason:   "stop",
		Attempts:       2,
		ErrorKind:      "ek",
		ErrorMessage:   "em",
		Upstream:       &sdkusage.UpstreamTiming{Start: 1, ResponseStart: 2, ResponseEnd: 3},
		Reasoning:      &sdkusage.ReasoningTiming{Start: 4, End: 5},
		RelayKeyHash:   "rkh",
		PolicyID:       "pid",
		ModelID:        "mid",
		RequestedModel: "gpt-4o",
		HostID:         "hid",
		HostKeyID:      "hkid",
		Tokens:         sdkusage.Tokens{"input": 10},
		Extras:         map[string]string{"client_ip": "1.2.3.4"},
		Tags:           map[string]string{"session_id": "s1"},
		Model:          "gpt-4o",
		Host:           "openai",
		Policy:         "default",
		Provider:       "openai",
		CostNanos:      ptr(int64(10_000_000)),
		CostBreakdown:  map[string]int64{"tokens.input": 10_000_000},
		Pricing:        "openai-gpt-4o",
	}

	got, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"request_id":"r1","source":"pipeline","ts":"2026-06-11T12:00:00Z",` +
		`"status":200,"duration_ms":42,"streamed":true,"finish_reason":"stop",` +
		`"attempts":2,"error_kind":"ek","error_message":"em",` +
		`"upstream":{"start":1,"response_start":2,"response_end":3},` +
		`"reasoning":{"start":4,"end":5},` +
		`"relay_key_hash":"rkh","policy_id":"pid","model_id":"mid",` +
		`"requested_model":"gpt-4o","host_id":"hid","host_key_id":"hkid",` +
		`"tokens":{"input":10},"extras":{"client_ip":"1.2.3.4"},` +
		`"tags":{"session_id":"s1"},` +
		`"model":"gpt-4o","host":"openai","policy":"default",` +
		`"provider":"openai",` +
		`"cost_nanos":10000000,"cost_breakdown":{"tokens.input":10000000},` +
		`"pricing":"openai-gpt-4o"}`
	if string(got) != want {
		t.Fatalf("marshal drift:\n got: %s\nwant: %s", got, want)
	}

	var back Event
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Model != "gpt-4o" || back.Host != "openai" || back.Policy != "default" {
		t.Fatalf("slug round-trip: %+v", back)
	}
	if back.Provider != "openai" || back.Pricing != "openai-gpt-4o" {
		t.Fatalf("provider/pricing round-trip: %+v", back)
	}
	if back.CostNanos == nil || *back.CostNanos != 10_000_000 ||
		back.CostBreakdown["tokens.input"] != 10_000_000 {
		t.Fatalf("cost round-trip: %+v", back)
	}
}

// Unpriced (CostNanos nil) and a true $0 (CostNanos = 0) must stay
// distinguishable through a JSON round-trip: the former omits the field,
// the latter emits an explicit 0.
func TestEvent_UnpricedVsZeroCostJSON(t *testing.T) {
	unpriced, err := json.Marshal(Event{RequestID: "u"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(unpriced) != `{"request_id":"u","source":"","ts":"0001-01-01T00:00:00Z","status":0,"duration_ms":0}` {
		t.Fatalf("unpriced row leaked cost fields: %s", unpriced)
	}

	zero, err := json.Marshal(Event{RequestID: "z", CostNanos: ptr(int64(0)), Pricing: "sheet"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Event
	if err := json.Unmarshal(zero, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.CostNanos == nil || *back.CostNanos != 0 {
		t.Fatalf("zero-cost row lost its stamp: %s → %+v", zero, back)
	}
}

func ptr[T any](v T) *T { return &v }

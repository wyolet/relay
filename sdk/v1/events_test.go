package v1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
)

// The wire form of each stream event is the contract every adapter's stream translator writes and reads.
func TestEvents_WireShape(t *testing.T) {
	tests := []struct {
		name  string
		event any
		wire  string
	}{
		{"generation.created", GenerationCreatedEvent{ID: "gen_1", Model: "m"}, `{"id":"gen_1","model":"m"}`},
		{"item.started message", ItemStartedEvent{ItemID: "i1", ItemType: ItemTypeMessage, Index: 0}, `{"item_id":"i1","item_type":"message","index":0}`},
		{"item.started function_call", ItemStartedEvent{ItemID: "i2", ItemType: ItemTypeFunctionCall, Index: 1, Name: "f", CallID: "c1"},
			`{"item_id":"i2","item_type":"function_call","index":1,"name":"f","call_id":"c1"}`},
		{"item.delta", ItemDeltaEvent{ItemID: "i1", Index: 0, Kind: DeltaKindText, Delta: ""}, `{"item_id":"i1","index":0,"kind":"text","delta":""}`},
		{"item.completed", ItemCompletedEvent{ItemID: "i1", Index: 2, Item: &Reasoning{ID: "i1"}}, `{"item_id":"i1","index":2,"item":{"type":"reasoning","id":"i1"}}`},
		{"generation.completed minimal", GenerationCompletedEvent{ID: "gen_1", Status: StatusCompleted}, `{"id":"gen_1","status":"completed"}`},
		{"generation.completed full", GenerationCompletedEvent{ID: "gen_1", Status: StatusIncomplete, FinishReason: FinishReasonLength,
			Usage: usage.Tokens{"input": 1}, ServiceTier: "flex", IncompleteDetails: &IncompleteDetails{Reason: "max_tokens"}, RelayUsage: &RelayUsage{Attempts: 1}},
			`{"id":"gen_1","status":"incomplete","finish_reason":"length","usage":{"input":1},"service_tier":"flex","incomplete_details":{"reason":"max_tokens"},"relay_usage":{"attempts":1}}`},
		{"error", ErrorEvent{Code: "upstream_error", Message: "boom"}, `{"code":"upstream_error","message":"boom"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantJSON(t, tc.event, tc.wire) })
	}
}

func TestItemCompletedEvent_UnmarshalJSON(t *testing.T) {
	stale := ItemCompletedEvent{ItemID: "stale", Index: 9, Item: &Reasoning{}}
	tests := []struct {
		name string
		wire string
		want ItemCompletedEvent
		err  string
	}{
		{name: "message item", wire: `{"item_id":"msg_1","index":2,"item":{"type":"message","role":"assistant","content":"hi"}}`,
			want: ItemCompletedEvent{ItemID: "msg_1", Index: 2, Item: &Message{Role: RoleAssistant, Content: []Part{&TextPart{Text: "hi"}}}}},
		{name: "function_call item", wire: `{"item_id":"fc","index":0,"item":{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}}`,
			want: ItemCompletedEvent{ItemID: "fc", Item: &FunctionCall{CallID: "c", Name: "f", Arguments: "{}"}}},
		{name: "null item", wire: `{"item_id":"x","index":0,"item":null}`, want: ItemCompletedEvent{ItemID: "x"}},
		{name: "absent item", wire: `{"item_id":"x","index":1}`, want: ItemCompletedEvent{ItemID: "x", Index: 1}},
		{name: "unsupported item", wire: `{"item_id":"x","index":0,"item":{"type":"computer_call"}}`,
			want: ItemCompletedEvent{ItemID: "x"}, err: `unsupported item type "computer_call"`},
		{name: "not an object", wire: `[]`, want: stale, err: "json: cannot unmarshal array…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A reused event must not keep the previous frame's item.
			got := stale
			wantErr(t, json.Unmarshal([]byte(tc.wire), &got), tc.err)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("event (-want +got):\n%s", diff)
			}
		})
	}
}

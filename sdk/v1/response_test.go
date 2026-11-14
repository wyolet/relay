package v1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
)

// fullResponse sets every Response field; fullResponseWire is its exact wire form.
func fullResponse() *Response {
	return &Response{
		ID: "resp_abc", Object: "response", CreatedAt: 1716000000, Model: "m",
		Status: StatusIncomplete, FinishReason: FinishReasonRefusal,
		Output: []Item{
			&Reasoning{ID: "rs_1", Summary: []SummaryText{{Text: "s"}}},
			&Message{ID: "msg_1", Status: StatusCompleted, Role: RoleAssistant, Content: []Part{&OutputTextPart{Text: "I can't help with that."}}},
			&FunctionCall{CallID: "c1", Name: "f", Arguments: "{}"},
		},
		Usage:             usage.Tokens{"input": 10, "output": 5},
		ServiceTier:       "priority",
		Error:             &Error{Code: "server_error", Message: "upstream failed"},
		IncompleteDetails: &IncompleteDetails{Reason: "max_output_tokens"},
		RelayUsage: &RelayUsage{RequestID: "req_1", Tokens: usage.Tokens{"input": 10}, FinishReason: FinishReasonStop, Attempts: 2, Streamed: true,
			Timing: &RelayTiming{Upstream: RelayUpstreamTiming{Start: 1, ResponseStart: 2, ResponseEnd: 3}, End: 4}},
		Extensions: map[string]json.RawMessage{"cache_hit": json.RawMessage(`true`)},
	}
}

const fullResponseWire = `{"id":"resp_abc","object":"response","created_at":1716000000,"model":"m","status":"incomplete","finish_reason":"refusal",` +
	`"output":[{"type":"reasoning","id":"rs_1","summary":[{"text":"s"}]},` +
	`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"I can't help with that."}]},` +
	`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],` +
	`"usage":{"input":10,"output":5},"service_tier":"priority","error":{"code":"server_error","message":"upstream failed"},` +
	`"incomplete_details":{"reason":"max_output_tokens"},` +
	`"relay_usage":{"request_id":"req_1","tokens":{"input":10},"finish_reason":"stop","attempts":2,"streamed":true,"timing":{"upstream":{"start":1,"response_start":2,"response_end":3},"end":4}},` +
	`"extensions":{"cache_hit":true}}`

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *Response
	}{
		{"every field", fullResponseWire, fullResponse()},
		{"minimal", `{"id":"r","object":"response","created_at":0,"model":"m","status":"completed"}`,
			&Response{ID: "r", Object: "response", Model: "m", Status: StatusCompleted}},
		{"null output stays nil", `{"id":"r","output":null}`, &Response{ID: "r"}},
		{"empty output", `{"id":"r","output":[]}`, &Response{ID: "r", Output: []Item{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseResponse([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseResponse: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("response (-want +got):\n%s", diff)
			}
		})
	}

	for _, tc := range []struct{ name, body, want string }{
		{"invalid JSON", `{`, "invalid JSON: unexpected end of JSON input"},
		{"output not an array", `{"output":{}}`, "output: input array: json: cannot unmarshal object…"},
		{"unsupported output item", `{"output":[{"type":"web_search_call"}]}`, `output: unsupported item type "web_search_call"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseResponse([]byte(tc.body))
			if got != nil {
				t.Errorf("response = %+v, want nil on error", got)
			}
			wantErr(t, err, tc.want)
		})
	}
}

func TestResponse_MarshalJSON(t *testing.T) {
	wantJSON(t, fullResponse(), fullResponseWire)
	// A bare response carries its five required members and nothing else.
	wantJSON(t, &Response{ID: "r", Object: "response", Model: "m", Status: StatusCompleted},
		`{"id":"r","object":"response","created_at":0,"model":"m","status":"completed"}`)
}

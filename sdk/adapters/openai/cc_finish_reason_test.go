package openai

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Each Chat Completions finish_reason, buffered and streamed, parses to the same canonical status and goes back out as back.
func TestCCFinishReason(t *testing.T) {
	cases := []struct {
		reason string
		want   string // canonical status, finish_reason and incomplete_details
		back   string
	}{
		{"stop", `{"status":"completed","finish_reason":"stop"}`, "stop"},
		{"", `{"status":"completed","finish_reason":"stop"}`, "stop"},
		{"length", `{"status":"incomplete","finish_reason":"length","incomplete_details":{"reason":"max_output_tokens"}}`, "length"},
		{"tool_calls", `{"status":"completed","finish_reason":"tool_calls"}`, "tool_calls"},
		{"function_call", `{"status":"completed","finish_reason":"tool_calls"}`, "tool_calls"},
		{"content_filter", `{"status":"completed","finish_reason":"content_filter"}`, "content_filter"},
		{"new_reason", `{"status":"incomplete","finish_reason":"other","incomplete_details":{"reason":"openai:new_reason"}}`, "new_reason"},
	}
	for _, tc := range cases {
		t.Run("buffered "+tc.reason, func(t *testing.T) {
			resp, err := CCTranslator{}.ParseResponse([]byte(ccResponse(`{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"`+tc.reason+`"}`, "")))
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, mustJSON(v1.GenerationCompletedEvent{Status: resp.Status, FinishReason: resp.FinishReason, IncompleteDetails: resp.IncompleteDetails}), `{"id":"",`+tc.want[1:])
			wire, err := CCTranslator{}.SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeMap(t, wire)["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != tc.back {
				t.Errorf("serialized finish_reason = %v, want %s", got, tc.back)
			}
		})
		t.Run("stream "+tc.reason, func(t *testing.T) {
			frames := []string{`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`}
			if tc.reason != "" {
				frames = append(frames, `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"`+tc.reason+`"}]}`)
			}
			canon := splitCanonicalFrames(runStream(t, CCTranslator{}.NewToCanonicalStream(), append(frames, "data: [DONE]")...))
			event, data, _ := v1.ParseSSEChunk(canon[len(canon)-1])
			if event != v1.EventGenerationCompleted {
				t.Fatalf("last event %s, want %s", event, v1.EventGenerationCompleted)
			}
			assertJSON(t, data, `{"id":"c1",`+tc.want[1:])
			wire := splitCanonicalFrames(runStream(t, CCTranslator{}.NewFromCanonicalStream(), "event: "+event+"\ndata: "+string(data)))
			_, final, _ := v1.ParseSSEChunk(wire[0])
			var chunk ChatStreamChunk
			if err := json.Unmarshal(final, &chunk); err != nil {
				t.Fatal(err)
			}
			if got := chunk.Choices[0].FinishReason; got == nil || *got != tc.back {
				t.Errorf("streamed back finish_reason = %v, want %s", got, tc.back)
			}
		})
	}
}

// A refusal lifts a stop and nothing else, buffered or streamed.
func TestCCRefusalLiftsOnlyStop(t *testing.T) {
	for reason, want := range map[string]v1.FinishReason{"stop": v1.FinishReasonRefusal, "length": v1.FinishReasonLength, "tool_calls": v1.FinishReasonToolCalls} {
		resp, err := CCTranslator{}.ParseResponse([]byte(ccResponse(`{"index":0,"message":{"role":"assistant","refusal":"no"},"finish_reason":"`+reason+`"}`, "")))
		if err != nil {
			t.Fatal(err)
		}
		if resp.FinishReason != want {
			t.Errorf("%s: buffered finish_reason = %s, want %s", reason, resp.FinishReason, want)
		}
		canon := splitCanonicalFrames(runStream(t, CCTranslator{}.NewToCanonicalStream(), ccChunk(`{"refusal":"no"}`),
			`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"`+reason+`"}]}`, "data: [DONE]"))
		_, data, _ := v1.ParseSSEChunk(canon[len(canon)-1])
		var ev v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil || ev.FinishReason != want {
			t.Errorf("%s: streamed finish_reason = %s (%v), want %s", reason, ev.FinishReason, err, want)
		}
	}
}

// Only a Chat Completions upstream's own raw reason is written back; any other unknown reason must not read as stop.
func TestCCSerialize_OtherFinishReason(t *testing.T) {
	cases := []struct {
		inc  *v1.IncompleteDetails
		want string
	}{
		{&v1.IncompleteDetails{Reason: "openai:new_reason"}, "new_reason"},
		{&v1.IncompleteDetails{Reason: "anthropic:new_reason"}, "content_filter"},
		{&v1.IncompleteDetails{Reason: "new_reason"}, "content_filter"},
		{&v1.IncompleteDetails{Reason: "openai:"}, "content_filter"},
		{nil, "content_filter"},
	}
	for _, tc := range cases {
		resp := &v1.Response{ID: "c1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: tc.inc}
		out, err := (CCTranslator{}).SerializeResponse(resp, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeMap(t, out)["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != tc.want {
			t.Errorf("%+v: buffered finish_reason = %v, want %s", tc.inc, got, tc.want)
		}
		if got := canonicalFinishReasonToCC(v1.FinishReasonOther, tc.inc); got != tc.want {
			t.Errorf("%+v: stream finish_reason = %s, want %s", tc.inc, got, tc.want)
		}
	}
}

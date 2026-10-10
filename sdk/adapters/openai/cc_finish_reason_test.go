package openai

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func ccFinishBody(reason string) []byte {
	return mustJSON(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "x"},
			"finish_reason": reason,
		}},
	})
}

// function_call is the legacy spelling of tool_calls.
func TestCCParseResponse_LegacyFunctionCallFinish(t *testing.T) {
	resp, err := (CCTranslator{}).ParseResponse(ccFinishBody("function_call"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonToolCalls || resp.Status != v1.StatusCompleted {
		t.Errorf("status/finish = %q/%q, want completed/tool_calls", resp.Status, resp.FinishReason)
	}
}

// An unknown finish_reason must not read as a clean stop (rule 11): it parses
// to incomplete/other and the raw value is kept.
func TestCCParseResponse_UnknownFinishNotSuccess(t *testing.T) {
	resp, err := (CCTranslator{}).ParseResponse(ccFinishBody("new_reason"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete || resp.FinishReason != v1.FinishReasonOther {
		t.Errorf("status/finish = %q/%q, want incomplete/other", resp.Status, resp.FinishReason)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "openai:new_reason" {
		t.Errorf("incomplete_details = %+v", resp.IncompleteDetails)
	}
	if got := string(resp.Extensions["openai.finish_reason"]); got != `"new_reason"` {
		t.Errorf(`extensions["openai.finish_reason"] = %s`, got)
	}

	out, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeMap(t, out)["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != "new_reason" {
		t.Errorf("round-trip finish_reason = %v, want the raw value, not stop", got)
	}
}

func ccStreamCompleted(t *testing.T, reason string) v1.GenerationCompletedEvent {
	t.Helper()
	fn := (CCTranslator{}).NewToCanonicalStream()
	var out []byte
	for _, c := range [][]byte{
		ccSSEChunk(map[string]any{"id": "c1", "model": "gpt-4o", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": "x"},
		}}}),
		ccSSEChunk(map[string]any{"id": "c1", "model": "gpt-4o", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": reason,
		}}}),
		ccDoneChunk(),
	} {
		b, err := fn(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	for _, frame := range splitCanonicalFrames(out) {
		if event, data, ok := v1.ParseSSEChunk(frame); ok && event == v1.EventGenerationCompleted {
			var ev v1.GenerationCompletedEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Fatal(err)
			}
			return ev
		}
	}
	t.Fatalf("no generation.completed: %s", out)
	return v1.GenerationCompletedEvent{}
}

func TestCCStream_LegacyFunctionCallFinish(t *testing.T) {
	if ev := ccStreamCompleted(t, "function_call"); ev.FinishReason != v1.FinishReasonToolCalls {
		t.Errorf("finish_reason = %q, want tool_calls", ev.FinishReason)
	}
}

func TestCCStream_UnknownFinishNotSuccess(t *testing.T) {
	ev := ccStreamCompleted(t, "new_reason")
	if ev.Status != v1.StatusIncomplete || ev.FinishReason != v1.FinishReasonOther {
		t.Errorf("status/finish = %q/%q, want incomplete/other", ev.Status, ev.FinishReason)
	}
	if ev.IncompleteDetails == nil || ev.IncompleteDetails.Reason != "openai:new_reason" {
		t.Errorf("incomplete_details = %+v, want the raw reason", ev.IncompleteDetails)
	}
}

// canonical other → CC finish_reason, buffered and streamed. Only a CC
// upstream's own raw reason is written back; nothing becomes "stop".
func TestCCSerialize_OtherFinishReason(t *testing.T) {
	cases := []struct {
		name string
		inc  *v1.IncompleteDetails
		want string
	}{
		{"own raw reason", &v1.IncompleteDetails{Reason: "openai:new_reason"}, "new_reason"},
		{"foreign raw reason", &v1.IncompleteDetails{Reason: "anthropic:new_reason"}, "content_filter"},
		{"unprefixed reason", &v1.IncompleteDetails{Reason: "new_reason"}, "content_filter"},
		{"no details", nil, "content_filter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &v1.Response{ID: "c1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: tc.inc}
			out, err := (CCTranslator{}).SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeMap(t, out)["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != tc.want {
				t.Errorf("buffered finish_reason = %v, want %s", got, tc.want)
			}
			if got := canonicalFinishReasonToCC(v1.FinishReasonOther, tc.inc); got != tc.want {
				t.Errorf("stream finish_reason = %s, want %s", got, tc.want)
			}
		})
	}
}

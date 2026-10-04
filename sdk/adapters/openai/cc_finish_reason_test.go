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

// An unknown finish_reason must not read as a clean stop (rule 11): the
// status turns incomplete, no finish_reason is fabricated, and the raw value
// is kept.
func TestCCParseResponse_UnknownFinishNotSuccess(t *testing.T) {
	resp, err := (CCTranslator{}).ParseResponse(ccFinishBody("new_reason"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete || resp.FinishReason == v1.FinishReasonStop {
		t.Errorf("status/finish = %q/%q, want incomplete and not stop", resp.Status, resp.FinishReason)
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
	if ev.Status != v1.StatusIncomplete || ev.FinishReason == v1.FinishReasonStop {
		t.Errorf("status/finish = %q/%q, want incomplete and not stop", ev.Status, ev.FinishReason)
	}
}

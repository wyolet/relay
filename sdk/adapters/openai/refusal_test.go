package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Refusal is a finish_reason, not an item type: its text rides a normal
// message item and the finish_reason carries the signal (rule 9).

const refusalText = "I cannot help with that."

func refusalCanonicalResponse() *v1.Response {
	return &v1.Response{
		ID:           "resp_ref",
		Model:        "gpt-4o",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonRefusal,
		Output: []v1.Item{&v1.Message{
			ID:      "msg_0",
			Role:    v1.RoleAssistant,
			Status:  v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: refusalText}},
		}},
	}
}

func assertRefusalMessage(t *testing.T, out []v1.Item) {
	t.Helper()
	for _, it := range out {
		m, ok := it.(*v1.Message)
		if !ok {
			continue
		}
		for _, p := range m.Content {
			if tp, ok := p.(*v1.OutputTextPart); ok && tp.Text == refusalText {
				return
			}
		}
	}
	t.Errorf("refusal text not carried as message output text: %#v", out)
}

// completedFinish returns the finish_reason on the stream's generation.completed frame.
func completedFinish(t *testing.T, canonical []byte) v1.FinishReason {
	t.Helper()
	for _, frame := range splitCanonicalFrames(canonical) {
		event, data, ok := v1.ParseSSEChunk(frame)
		if !ok || event != v1.EventGenerationCompleted {
			continue
		}
		var ev v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		return ev.FinishReason
	}
	t.Fatalf("no generation.completed in %s", canonical)
	return ""
}

func TestCCRefusal_ParseResponse(t *testing.T) {
	body := mustJSON(map[string]any{
		"id": "chatcmpl-ref", "object": "chat.completion", "model": "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": nil, "refusal": refusalText},
			"finish_reason": "stop",
		}},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonRefusal {
		t.Errorf("finish_reason = %q, want refusal", resp.FinishReason)
	}
	assertRefusalMessage(t, resp.Output)
}

func TestCCRefusal_SerializeResponse(t *testing.T) {
	b, err := (CCTranslator{}).SerializeResponse(refusalCanonicalResponse(), nil)
	if err != nil {
		t.Fatal(err)
	}
	choice := decodeMap(t, b)["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["refusal"] != refusalText {
		t.Errorf("message.refusal = %v", msg["refusal"])
	}
	if msg["content"] != nil {
		t.Errorf("message.content = %v, want null alongside refusal", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop (CC's refusal pairing)", choice["finish_reason"])
	}

	// Round-trip back to canonical keeps the refusal.
	resp, err := (CCTranslator{}).ParseResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonRefusal {
		t.Errorf("round-trip finish_reason = %q", resp.FinishReason)
	}
}

func TestCCRefusal_SerializeResponseWithoutText(t *testing.T) {
	resp := refusalCanonicalResponse()
	resp.Output = nil
	b, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	choice := decodeMap(t, b)["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "content_filter" {
		t.Errorf("finish_reason = %v, want content_filter for a textless refusal", choice["finish_reason"])
	}
}

func TestCCRefusal_ToCanonicalStream(t *testing.T) {
	fn := (CCTranslator{}).NewToCanonicalStream()
	var out []byte
	for _, c := range [][]byte{
		ccSSEChunk(map[string]any{"id": "c1", "model": "gpt-4o", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "refusal": refusalText},
		}}}),
		ccSSEChunk(map[string]any{"id": "c1", "model": "gpt-4o", "choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}}),
		ccDoneChunk(),
	} {
		b, err := fn(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	if got := completedFinish(t, out); got != v1.FinishReasonRefusal {
		t.Errorf("finish_reason = %q, want refusal", got)
	}
	if !strings.Contains(string(out), refusalText) {
		t.Errorf("refusal text missing from canonical stream: %s", out)
	}
}

func TestCCRefusal_FromCanonicalStream(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()
	var out []byte
	feed := func(event string, data any) {
		b, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "r1", Model: "m"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Kind: v1.DeltaKindText, Delta: refusalText})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "r1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonRefusal})

	if !strings.Contains(string(out), `"finish_reason":"content_filter"`) {
		t.Errorf("refusal must not stream as a clean stop: %s", out)
	}
}

func TestResponsesRefusal_ParseResponse(t *testing.T) {
	resp, err := (ResponsesTranslator{}).ParseResponse(responsesRefusalBody())
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonRefusal {
		t.Errorf("finish_reason = %q, want refusal", resp.FinishReason)
	}
	assertRefusalMessage(t, resp.Output)
}

func TestResponsesRefusal_SerializeResponse(t *testing.T) {
	b, err := (ResponsesTranslator{}).SerializeResponse(refusalCanonicalResponse(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"refusal"`) || !strings.Contains(string(b), refusalText) {
		t.Errorf("want a refusal content part: %s", b)
	}
	resp, err := (ResponsesTranslator{}).ParseResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonRefusal {
		t.Errorf("round-trip finish_reason = %q", resp.FinishReason)
	}
}

func TestResponsesRefusal_ToCanonicalStream(t *testing.T) {
	fn := (ResponsesTranslator{}).NewToCanonicalStream()
	var body map[string]any
	if err := json.Unmarshal(responsesRefusalBody(), &body); err != nil {
		t.Fatal(err)
	}
	out, err := fn(responsesSSEChunk(ResponsesEventCompleted, map[string]any{"type": ResponsesEventCompleted, "response": body}))
	if err != nil {
		t.Fatal(err)
	}
	if got := completedFinish(t, out); got != v1.FinishReasonRefusal {
		t.Errorf("finish_reason = %q, want refusal", got)
	}
}

func TestResponsesRefusal_FromCanonicalStream(t *testing.T) {
	fn := (ResponsesTranslator{}).NewFromCanonicalStream()
	var out []byte
	feed := func(event string, data any) {
		b, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	msg := &v1.Message{ID: "msg_0", Role: v1.RoleAssistant, Status: v1.StatusCompleted, Content: []v1.Part{&v1.OutputTextPart{Text: refusalText}}}
	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "r1", Model: "m"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Kind: v1.DeltaKindText, Delta: refusalText})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_0", Item: msg})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "r1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonRefusal})

	terminal := out[strings.LastIndex(string(out), "event: "+ResponsesEventCompleted):]
	if !strings.Contains(string(terminal), `"type":"refusal"`) {
		t.Errorf("terminal response must type the refusal: %s", terminal)
	}
}

func responsesRefusalBody() []byte {
	return mustJSON(map[string]any{
		"id": "resp_ref", "object": "response", "model": "gpt-5", "status": "completed",
		"output": []any{map[string]any{
			"type": "message", "id": "msg_0", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "refusal", "refusal": refusalText}},
		}},
	})
}

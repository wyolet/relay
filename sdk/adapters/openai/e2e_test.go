package openai

import (
	"testing"
)

func TestE2E_ResponsesViaCC(t *testing.T) {
	// 1. Responses wire request.
	inputBytes := mustJSON(map[string]any{
		"model":       "gpt-4o",
		"input":       "hello",
		"temperature": 0.5,
	})

	// 2. Parse to canonical via ResponsesTranslator.
	rt := ResponsesTranslator{}
	canonReq, err := rt.ParseRequest(inputBytes)
	if err != nil {
		t.Fatalf("ResponsesTranslator.ParseRequest: %v", err)
	}
	if len(canonReq.Model) != 1 || canonReq.Model[0] != "gpt-4o" {
		t.Errorf("model: %v", canonReq.Model)
	}

	// 3. Serialize to CC wire via CCTranslator.
	ccT := CCTranslator{}
	ccBody, err := ccT.SerializeRequest(canonReq)
	if err != nil {
		t.Fatalf("CCTranslator.SerializeRequest: %v", err)
	}
	ccMap := decodeMap(t, ccBody)
	if ccMap["model"] != "gpt-4o" {
		t.Errorf("cc model: %v", ccMap["model"])
	}
	// Should have messages array (not input array).
	if _, ok := ccMap["messages"]; !ok {
		t.Error("cc body missing messages field")
	}

	// 4. Mock CC response.
	ccResp := mustJSON(map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Hello there!"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})

	// 5. Parse CC response to canonical.
	canonResp, err := ccT.ParseResponse(ccResp)
	if err != nil {
		t.Fatalf("CCTranslator.ParseResponse: %v", err)
	}

	// 6. Serialize canonical → Responses wire.
	respBytes, err := rt.SerializeResponse(canonResp, canonReq)
	if err != nil {
		t.Fatalf("ResponsesTranslator.SerializeResponse: %v", err)
	}

	// 7. Verify well-formed Responses response with echo fields.
	m := decodeMap(t, respBytes)
	if m["object"] != "response" {
		t.Errorf("object: %v", m["object"])
	}
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	if m["model"] != "gpt-4o" {
		t.Errorf("model: %v", m["model"])
	}
	// Temperature was in original Responses request → should echo.
	if m["temperature"] != 0.5 {
		t.Errorf("temperature echo: %v", m["temperature"])
	}
	output, ok := m["output"].([]any)
	if !ok || len(output) == 0 {
		t.Errorf("output: %v", m["output"])
	}
}

func TestE2E_StreamingCCToResponses(t *testing.T) {
	ccToCanon := (CCTranslator{}).NewToCanonicalStream()
	canonToResp := (ResponsesTranslator{}).NewFromCanonicalStream()
	if canonToResp == nil {
		t.Fatal("NewFromCanonicalStream returned nil")
	}

	cs := NewComposedStream(ccToCanon, canonToResp)

	ccChunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-e2e",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "Hello"},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-e2e",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": " world"},
			}},
		}),
		ccDoneChunk(),
	}

	var allFrames []ResponsesSSEFrame
	for _, c := range ccChunks {
		frames, err := cs.Translate(c)
		if err != nil {
			t.Fatalf("ComposedStream.Translate: %v", err)
		}
		allFrames = append(allFrames, frames...)
	}

	// Composed stream must produce at least response.created and response.completed.
	var eventNames []string
	for _, f := range allFrames {
		eventNames = append(eventNames, f.Event)
	}

	wantContains := []string{ResponsesEventCreated, ResponsesEventCompleted}
	for _, want := range wantContains {
		found := false
		for _, e := range eventNames {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing Responses event %q in composed stream output %v", want, eventNames)
		}
	}

	// Every frame must have non-empty data.
	for i, f := range allFrames {
		if len(f.Data) == 0 {
			t.Errorf("frame[%d] event=%q has empty data", i, f.Event)
		}
	}
}

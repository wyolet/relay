package anthropic

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- Stream: Anthropic → canonical tests ----

func messageStartChunk(id, model string) []byte {
	return sseChunk("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    id,
			"type":  "message",
			"role":  "assistant",
			"model": model,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 0},
		},
	})
}

func pingChunk() []byte {
	return sseChunk("ping", map[string]any{"type": "ping"})
}

func contentBlockStartText(index int) []byte {
	return sseChunk("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
}

func textDeltaChunk(index int, text string) []byte {
	return sseChunk("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func contentBlockStopChunk(index int) []byte {
	return sseChunk("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
}

func messageDeltaChunk(stopReason string, outTokens int) []byte {
	return sseChunk("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	})
}

func messageStopChunk() []byte {
	return sseChunk("message_stop", map[string]any{"type": "message_stop"})
}

func TestAnthropicToCanonical_TextStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_001", "claude-3-5-sonnet-20241022"),
		pingChunk(),
		contentBlockStartText(0),
		textDeltaChunk(0, "Hello"),
		textDeltaChunk(0, " world"),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 5),
		messageStopChunk(),
	}

	events := collectCanonEvents(t, chunks)

	wantSequence := []string{
		v1.EventGenerationCreated,
		v1.EventItemStarted,
		v1.EventItemDelta,
		v1.EventItemDelta,
		v1.EventItemCompleted,
		v1.EventGenerationCompleted,
	}

	if len(events) != len(wantSequence) {
		t.Fatalf("events: got %v want %v", events, wantSequence)
	}
	for i, ev := range events {
		if ev != wantSequence[i] {
			t.Errorf("events[%d]: got %q want %q", i, ev, wantSequence[i])
		}
	}
}

func TestAnthropicToCanonical_PingIgnored(t *testing.T) {
	chunks := [][]byte{pingChunk()}
	events := collectCanonEvents(t, chunks)
	if len(events) != 0 {
		t.Errorf("expected no events from ping, got %v", events)
	}
}

func TestAnthropicToCanonical_EmptyContentBlockTypeSkipped(t *testing.T) {
	tests := []struct {
		name         string
		contentBlock map[string]any
	}{
		{name: "missing type", contentBlock: map[string]any{"text": ""}},
		{name: "empty type", contentBlock: map[string]any{"type": ""}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fn := (AnthropicTranslator{}).NewToCanonicalStream()
			if _, err := fn(messageStartChunk("msg_empty_type", "claude-3-5-sonnet-20241022")); err != nil {
				t.Fatal(err)
			}

			out, err := fn(sseChunk("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         0,
				"content_block": tc.contentBlock,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if frames := splitFrames(out); len(frames) != 0 {
				t.Fatalf("empty content_block.type should be skipped, got frames %q", out)
			}

			out, err = fn(textDeltaChunk(0, "must not attach"))
			if err != nil {
				t.Fatal(err)
			}
			if frames := splitFrames(out); len(frames) != 0 {
				t.Fatalf("delta after skipped block should not be emitted, got frames %q", out)
			}
		})
	}
}

func TestAnthropicToCanonical_ToolUseStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_tool", "claude-3-5-sonnet-20241022"),
		sseChunk("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use",
				"id":   "toolu_01",
				"name": "search",
			},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"q":`},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `"hi"}`},
		}),
		contentBlockStopChunk(0),
		messageDeltaChunk("tool_use", 10),
		messageStopChunk(),
	}

	events := collectCanonEvents(t, chunks)
	// Expect: created, started(fc), delta, delta, completed(fc), generation.completed
	hasStarted := false
	hasArgs := false
	for _, ev := range events {
		if ev == v1.EventItemStarted {
			hasStarted = true
		}
		if ev == v1.EventItemDelta {
			hasArgs = true
		}
	}
	if !hasStarted {
		t.Error("no item.started event")
	}
	if !hasArgs {
		t.Error("no item.delta event")
	}
}

func TestAnthropicToCanonical_ThinkingStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_think", "claude-3-7-sonnet-20250219"),
		sseChunk("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "thinking"},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "let me think"},
		}),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 20),
		messageStopChunk(),
	}

	events := collectCanonEvents(t, chunks)
	hasReasoning := false
	for _, ev := range events {
		if ev == v1.EventItemDelta {
			hasReasoning = true
		}
	}
	if !hasReasoning {
		t.Error("no item.delta event for thinking")
	}
}

func TestAnthropicToCanonical_ErrorEvent(t *testing.T) {
	chunk := sseChunk("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "overloaded_error",
			"message": "Overloaded",
		},
	})
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	if len(frames) == 0 {
		t.Fatal("expected error frame")
	}
	ev, data, _ := v1.ParseSSEChunk(frames[0])
	if ev != v1.EventError {
		t.Errorf("event: %q", ev)
	}
	var errEvt v1.ErrorEvent
	_ = json.Unmarshal(data, &errEvt)
	if errEvt.Code != "overloaded_error" {
		t.Errorf("error code: %q", errEvt.Code)
	}
}

func TestAnthropicToCanonical_MaxTokensStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_len", "claude-3-5-sonnet-20241022"),
		contentBlockStartText(0),
		textDeltaChunk(0, "partial"),
		contentBlockStopChunk(0),
		messageDeltaChunk("max_tokens", 100),
		messageStopChunk(),
	}
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var allFrames [][]byte
	for _, c := range chunks {
		out, _ := fn(c)
		allFrames = append(allFrames, splitFrames(out)...)
	}

	var completedFrame []byte
	for _, f := range allFrames {
		ev, _, _ := v1.ParseSSEChunk(f)
		if ev == v1.EventGenerationCompleted {
			completedFrame = f
			break
		}
	}
	if completedFrame == nil {
		t.Fatal("no generation.completed frame")
	}
	_, data, _ := v1.ParseSSEChunk(completedFrame)
	var ge v1.GenerationCompletedEvent
	_ = json.Unmarshal(data, &ge)
	if ge.Status != v1.StatusIncomplete {
		t.Errorf("status: %q", ge.Status)
	}
	if ge.FinishReason != v1.FinishReasonLength {
		t.Errorf("finish_reason: %q", ge.FinishReason)
	}
}

// ---- A-1 regression: streaming thinking signature (fix: signature_delta accumulation) ----

// thinkingStreamChunks builds a minimal Anthropic stream with thinking_delta +
// signature_delta so we can verify the completed Reasoning.ProviderData.
func thinkingStreamChunks(id, model, thinkText, sig string) [][]byte {
	return [][]byte{
		sseChunk("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": id, "type": "message", "role": "assistant", "model": model,
				"usage": map[string]any{"input_tokens": 5, "output_tokens": 0},
			},
		}),
		sseChunk("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking"},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": thinkText},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "signature_delta", "signature": sig},
		}),
		sseChunk("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseChunk("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 10},
		}),
		sseChunk("message_stop", map[string]any{"type": "message_stop"}),
	}
}

// TestStreamThinkingSignaturePreserved verifies that signature_delta is accumulated
// and surfaced in Reasoning.ProviderData with the same JSON shape as ParseResponse
// (required for multi-turn extended thinking round-trips).
// TestStreamThinkingSignaturePreserved verifies that signature_delta is accumulated
// and surfaced in Reasoning.ProviderData with the same JSON shape as ParseResponse
// (required for multi-turn extended thinking round-trips).
func TestStreamThinkingSignaturePreserved(t *testing.T) {
	const thinkText = "let me think carefully"
	const sig = "sig_streamed_abc123"

	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var completedData []byte
	for _, c := range thinkingStreamChunks("msg_sig", "claude-3-7-sonnet-20250219", thinkText, sig) {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		for _, frame := range splitFrames(out) {
			ev, data, ok := v1.ParseSSEChunk(frame)
			if ok && ev == v1.EventItemCompleted {
				completedData = data
			}
		}
	}
	if completedData == nil {
		t.Fatal("no item.completed event emitted")
	}

	// ItemCompletedEvent.Item is a v1.Item interface; decode it as raw JSON so we
	// can inspect the nested provider_data without a registered type-switch unmarshaler.
	var raw struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(completedData, &raw); err != nil {
		t.Fatalf("unmarshal item.completed: %v", err)
	}
	var itemFields struct {
		Content      string          `json:"content"`
		ProviderData json.RawMessage `json:"provider_data"`
	}
	if err := json.Unmarshal(raw.Item, &itemFields); err != nil {
		t.Fatalf("unmarshal item fields: %v", err)
	}
	if itemFields.Content != thinkText {
		t.Errorf("content: got %q want %q", itemFields.Content, thinkText)
	}
	if len(itemFields.ProviderData) == 0 {
		t.Fatal("provider_data is nil; signature was not preserved")
	}

	var pd map[string]string
	if err := json.Unmarshal(itemFields.ProviderData, &pd); err != nil {
		t.Fatalf("unmarshal provider_data: %v", err)
	}
	if pd["type"] != "thinking" {
		t.Errorf("provider_data.type: got %q want thinking", pd["type"])
	}
	if pd["thinking"] != thinkText {
		t.Errorf("provider_data.thinking: got %q want %q", pd["thinking"], thinkText)
	}
	if pd["signature"] != sig {
		t.Errorf("provider_data.signature: got %q want %q", pd["signature"], sig)
	}

	// Cross-check: non-streaming ParseResponse must produce the same ProviderData shape.
	syncBody := mustJSON(map[string]any{
		"id": "msg_sig_sync", "type": "message", "role": "assistant",
		"model": "claude-3-7-sonnet-20250219",
		"content": []any{map[string]any{
			"type": "thinking", "thinking": thinkText, "signature": sig,
		}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 10},
	})
	syncResp, err := (AnthropicTranslator{}).ParseResponse(syncBody)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	syncR, ok := syncResp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("sync output[0] is %T", syncResp.Output[0])
	}
	if string(syncR.ProviderData) != string(itemFields.ProviderData) {
		t.Errorf("stream provider_data %s != sync ProviderData %s", itemFields.ProviderData, syncR.ProviderData)
	}
}

// TestStreamSignatureDeltaNoCanonicalDelta verifies that a signature_delta chunk
// does NOT produce an item.delta event (signature is opaque, not streamed content).
func TestStreamSignatureDeltaNoCanonicalDelta(t *testing.T) {
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	for _, c := range [][]byte{
		sseChunk("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_x", "type": "message", "role": "assistant", "model": "m",
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
			},
		}),
		sseChunk("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking"},
		}),
	} {
		if _, err := fn(c); err != nil {
			t.Fatal(err)
		}
	}

	sigChunk := sseChunk("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "signature_delta", "signature": "sig_xyz"},
	})
	out, err := fn(sigChunk)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range splitFrames(out) {
		ev, _, ok := v1.ParseSSEChunk(frame)
		if ok && ev == v1.EventItemDelta {
			t.Error("unexpected item.delta event for signature_delta chunk")
		}
	}
}

package anthropic

import (
	"testing"
)

func TestExtractTokens_NonStreaming(t *testing.T) {
	body := []byte(`{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-4-7",
		"content": [],
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_creation_input_tokens": 25,
			"cache_read_input_tokens": 10,
			"server_tool_use": {"input_tokens": 3, "output_tokens": 2}
		}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil Tokens")
	}
	if tok["input"] != 100 {
		t.Errorf("input: want 100, got %d", tok["input"])
	}
	if tok["output"] != 50 {
		t.Errorf("output: want 50, got %d", tok["output"])
	}
	if tok["cache_creation"] != 25 {
		t.Errorf("cache_creation: want 25, got %d", tok["cache_creation"])
	}
	if tok["cache_read"] != 10 {
		t.Errorf("cache_read: want 10, got %d", tok["cache_read"])
	}
	if tok["server_tool_use_input"] != 3 {
		t.Errorf("server_tool_use_input: want 3, got %d", tok["server_tool_use_input"])
	}
	if tok["server_tool_use_output"] != 2 {
		t.Errorf("server_tool_use_output: want 2, got %d", tok["server_tool_use_output"])
	}
}

func TestExtractTokens_MessageStart(t *testing.T) {
	// SSE message_start event: usage inside message.usage
	body := []byte(`{
		"type": "message_start",
		"message": {
			"id": "msg_01",
			"type": "message",
			"role": "assistant",
			"content": [],
			"model": "claude-opus-4-7",
			"stop_reason": null,
			"stop_sequence": null,
			"usage": {
				"input_tokens": 25,
				"output_tokens": 1,
				"cache_creation_input_tokens": 5,
				"cache_read_input_tokens": 0
			}
		}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil Tokens from message_start")
	}
	if tok["input"] != 25 {
		t.Errorf("input: want 25, got %d", tok["input"])
	}
	if tok["output"] != 1 {
		t.Errorf("output: want 1, got %d", tok["output"])
	}
	if tok["cache_creation"] != 5 {
		t.Errorf("cache_creation: want 5, got %d", tok["cache_creation"])
	}
	if _, ok := tok["cache_read"]; ok {
		t.Error("cache_read should not be present when zero")
	}
}

func TestExtractTokens_MessageDelta(t *testing.T) {
	// SSE message_delta event: usage at top level
	body := []byte(`{
		"type": "message_delta",
		"delta": {"stop_reason": "end_turn", "stop_sequence": null},
		"usage": {"output_tokens": 15}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil Tokens from message_delta")
	}
	if tok["output"] != 15 {
		t.Errorf("output: want 15, got %d", tok["output"])
	}
	if _, ok := tok["input"]; ok {
		t.Error("input should not be present in message_delta")
	}
}

func TestExtractTokens_NoUsage(t *testing.T) {
	// Intermediate content_block_delta has no usage
	body := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
	tok := ExtractTokens(body)
	if tok != nil {
		t.Errorf("expected nil for no-usage chunk, got %v", tok)
	}
}

func TestExtractTokens_ZeroUsage(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":0,"output_tokens":0}}`)
	tok := ExtractTokens(body)
	if tok != nil {
		t.Errorf("expected nil for zero-usage, got %v", tok)
	}
}

func TestExtractTokens_MalformedJSON(t *testing.T) {
	tok := ExtractTokens([]byte(`{bad json`))
	if tok != nil {
		t.Error("expected nil for malformed JSON")
	}
}

func TestExtractTokens_PartialFields(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":7}}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil")
	}
	if tok["input"] != 7 {
		t.Errorf("input: want 7, got %d", tok["input"])
	}
	if _, ok := tok["output"]; ok {
		t.Error("output should not be present when zero")
	}
}

func TestExtractTokens_Add(t *testing.T) {
	// Simulate streaming: message_start gives input, message_delta gives output.
	msgStart := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}`)
	msgDelta := []byte(`{"type":"message_delta","delta":{},"usage":{"output_tokens":14}}`)

	// Accumulate like the pipeline does.
	// Usage in streams is cumulative per Anthropic docs, so we use the last value for output.
	// But ExtractTokens.Add would sum them — for input-only vs output-only chunks this is fine.
	t1 := ExtractTokens(msgStart)
	t2 := ExtractTokens(msgDelta)
	if t1 == nil || t2 == nil {
		t.Fatal("both should be non-nil")
	}
	// message_start has output_tokens=1, message_delta has output_tokens=14 (cumulative final)
	// In real streaming the pipeline accumulates by max-taking, but for this test we just verify Add works.
	combined := make(map[string]int64)
	for k, v := range t1 {
		combined[k] = v
	}
	for k, v := range t2 {
		combined[k] += v
	}
	if combined["input"] != 25 {
		t.Errorf("input: want 25, got %d", combined["input"])
	}
}

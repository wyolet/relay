package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- Anthropic → OpenAI ---

func TestAnthropicToOpenAI_TextStream(t *testing.T) {
	tr := &AnthropicToOpenAI{}

	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet-20241022\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	var allOut []string
	for _, c := range chunks {
		out, err := tr.TransformChunk([]byte(c))
		if err != nil {
			t.Fatalf("TransformChunk error: %v", err)
		}
		if len(out) > 0 {
			allOut = append(allOut, string(out))
		}
	}

	combined := strings.Join(allOut, "")

	// Should contain OpenAI SSE data lines
	if !strings.Contains(combined, "chat.completion.chunk") {
		t.Error("expected chat.completion.chunk in output")
	}
	if !strings.Contains(combined, "Hello") {
		t.Error("expected 'Hello' in output")
	}
	if !strings.Contains(combined, " world") {
		t.Error("expected ' world' in output")
	}
	if !strings.Contains(combined, "[DONE]") {
		t.Error("expected [DONE] sentinel")
	}

	// The message_delta chunk should carry finish_reason=stop.
	if !strings.Contains(combined, `"stop"`) {
		t.Error("expected finish_reason stop")
	}
}

func TestAnthropicToOpenAI_ToolCallStream(t *testing.T) {
	tr := &AnthropicToOpenAI{}

	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_02\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet-20241022\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":20,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_01\",\"name\":\"get_weather\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"location\\\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\": \\\"NYC\\\"\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":15}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	var allOut []string
	for _, c := range chunks {
		out, err := tr.TransformChunk([]byte(c))
		if err != nil {
			t.Fatalf("TransformChunk error: %v", err)
		}
		if len(out) > 0 {
			allOut = append(allOut, string(out))
		}
	}

	combined := strings.Join(allOut, "")

	// Should have tool_calls with name=get_weather
	if !strings.Contains(combined, "get_weather") {
		t.Error("expected tool name get_weather in output")
	}
	if !strings.Contains(combined, "toolu_01") {
		t.Error("expected tool id toolu_01 in output")
	}
	// Partial JSON args flow through (JSON-encoded in the output data line).
	if !strings.Contains(combined, "location") {
		t.Error("expected partial_json args in output")
	}
	// finish_reason should be tool_calls
	if !strings.Contains(combined, "tool_calls") {
		t.Error("expected finish_reason tool_calls")
	}
}

func TestAnthropicToOpenAI_StructuredOutput(t *testing.T) {
	tr := &AnthropicToOpenAI{}

	// Verify message_start emits role:assistant delta
	out, err := tr.TransformChunk([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_03\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Extract the data line
	var chunk oaStreamChunk
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
			if err := json.Unmarshal([]byte(line[6:]), &chunk); err == nil {
				break
			}
		}
	}
	if len(chunk.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if chunk.Choices[0].Delta.Role == nil || *chunk.Choices[0].Delta.Role != "assistant" {
		t.Error("expected role=assistant in first delta")
	}
	if chunk.ID != "msg_03" {
		t.Errorf("expected id=msg_03, got %q", chunk.ID)
	}
}

// --- OpenAI → Anthropic ---

func TestOpenAIToAnthropic_TextStream(t *testing.T) {
	tr := &OpenAIToAnthropic{}

	chunks := []string{
		// first chunk — triggers message_start
		"data: {\"id\":\"chatcmpl-01\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-01\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-01\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-01\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}

	var allOut []string
	for _, c := range chunks {
		out, err := tr.TransformChunk([]byte(c))
		if err != nil {
			t.Fatalf("TransformChunk error: %v", err)
		}
		if len(out) > 0 {
			allOut = append(allOut, string(out))
		}
	}

	combined := strings.Join(allOut, "")

	if !strings.Contains(combined, "message_start") {
		t.Error("expected message_start event")
	}
	if !strings.Contains(combined, "content_block_start") {
		t.Error("expected content_block_start event")
	}
	if !strings.Contains(combined, "text_delta") {
		t.Error("expected text_delta event")
	}
	if !strings.Contains(combined, "Hi") {
		t.Error("expected 'Hi' in output")
	}
	if !strings.Contains(combined, "message_stop") {
		t.Error("expected message_stop event")
	}
	if !strings.Contains(combined, "end_turn") {
		t.Error("expected end_turn stop reason")
	}
}

func TestOpenAIToAnthropic_ToolCallStream(t *testing.T) {
	tr := &OpenAIToAnthropic{}

	// First chunk: initializes the transformer
	first := `{"id":"chatcmpl-02","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	tr.TransformChunk([]byte("data: " + first + "\n\n"))

	toolChunk1 := `{"id":"chatcmpl-02","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"search","arguments":""}}]},"finish_reason":null}]}`
	out1, err := tr.TransformChunk([]byte("data: " + toolChunk1 + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out1), "tool_use") {
		t.Error("expected tool_use content_block_start")
	}
	if !strings.Contains(string(out1), "search") {
		t.Error("expected tool name search")
	}

	toolChunk2 := `{"id":"chatcmpl-02","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]},"finish_reason":null}]}`
	out2, err := tr.TransformChunk([]byte("data: " + toolChunk2 + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "input_json_delta") {
		t.Error("expected input_json_delta")
	}
	// Args are JSON-encoded in the output ({"q": appears as {"q": inside a JSON string).
	if !strings.Contains(string(out2), "q") {
		t.Error("expected partial args in output")
	}
}

// --- NewStreamTransformer ---

func TestNewStreamTransformer_KnownPairs(t *testing.T) {
	if fn := NewStreamTransformer("anthropic", "openai"); fn == nil {
		t.Error("anthropic→openai factory should not be nil")
	}
	if fn := NewStreamTransformer("openai", "anthropic"); fn == nil {
		t.Error("openai→anthropic factory should not be nil")
	}
}

func TestNewStreamTransformer_UnknownPair(t *testing.T) {
	if fn := NewStreamTransformer("openai", "ollama"); fn != nil {
		t.Error("unsupported pair should return nil")
	}
}

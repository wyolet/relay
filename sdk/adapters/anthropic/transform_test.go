package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/openai"
)

func ptr[T any](v T) *T { return &v }

// TestFromOpenAI_ToAnthropic converts a FullChatRequest (with system + tools)
// into a MessagesRequest and checks key fields.
func TestFromOpenAI_ToAnthropic(t *testing.T) {
	temp := 0.7
	req := &openai.FullChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []openai.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"You are helpful."`)},
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		Temperature: &temp,
		MaxTokens:   ptr(1024),
		Stop:        json.RawMessage(`["STOP","END"]`),
		Stream:      ptr(true),
		Tools: []openai.Tool{
			{
				Type: "function",
				Function: openai.FunctionDef{
					Name:        "get_weather",
					Description: "Get weather",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
				},
			},
		},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`),
	}

	got, err := FromOpenAI(req)
	if err != nil {
		t.Fatalf("FromOpenAI: %v", err)
	}

	if got.Model != req.Model {
		t.Errorf("model: want %q got %q", req.Model, got.Model)
	}
	if got.Temperature == nil || *got.Temperature != temp {
		t.Errorf("temperature: want %v got %v", temp, got.Temperature)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("max_tokens: want 1024 got %d", got.MaxTokens)
	}
	if !got.Stream {
		t.Error("stream should be true")
	}
	if len(got.StopSequences) != 2 {
		t.Errorf("stop_sequences: want 2 got %d", len(got.StopSequences))
	}

	// System should be extracted from the system message.
	var sys string
	if err := json.Unmarshal(got.System, &sys); err != nil || sys != "You are helpful." {
		t.Errorf("system: want %q got %q", "You are helpful.", sys)
	}

	// Only the user message should remain.
	if len(got.Messages) != 1 {
		t.Errorf("messages: want 1 got %d", len(got.Messages))
	}

	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Errorf("tools: unexpected %+v", got.Tools)
	}

	// tool_choice should be mapped to {type:"tool", name:"get_weather"}.
	var tc map[string]string
	if err := json.Unmarshal(got.ToolChoice, &tc); err != nil {
		t.Fatalf("tool_choice unmarshal: %v", err)
	}
	if tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Errorf("tool_choice: want type=tool name=get_weather, got %v", tc)
	}
}

// TestToOpenAI_FromAnthropic converts a MessagesRequest into a FullChatRequest.
func TestToOpenAI_FromAnthropic(t *testing.T) {
	msgRaw, _ := json.Marshal(map[string]string{"role": "user", "content": "hi"})
	req := &MessagesRequest{
		Model:     "claude-3-5-sonnet-20241022",
		MaxTokens: 512,
		System:    json.RawMessage(`"Be concise."`),
		Messages:  []json.RawMessage{msgRaw},
		Tools: []Tool{
			{
				Name:        "calc",
				Description: "calculator",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
	}

	got, err := ToOpenAI(req)
	if err != nil {
		t.Fatalf("ToOpenAI: %v", err)
	}

	if got.Model != req.Model {
		t.Errorf("model: want %q got %q", req.Model, got.Model)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 512 {
		t.Errorf("max_tokens: want 512 got %v", got.MaxTokens)
	}

	// First message should be the system message injected from System.
	if len(got.Messages) < 2 {
		t.Fatalf("messages: want ≥2 got %d", len(got.Messages))
	}
	if got.Messages[0].Role != "system" {
		t.Errorf("messages[0].role: want system got %s", got.Messages[0].Role)
	}

	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "calc" {
		t.Errorf("tools: unexpected %+v", got.Tools)
	}

	// Anthropic "any" → OpenAI "required"
	var tc string
	if err := json.Unmarshal(got.ToolChoice, &tc); err != nil || tc != "required" {
		t.Errorf("tool_choice: want %q got %q", "required", tc)
	}
}

// TestToOpenAIResponse maps Anthropic response → OpenAI ChatResponse.
func TestToOpenAIResponse(t *testing.T) {
	resp := &MessagesResponse{
		ID:    "msg_01XFDUDYJgAACzvnptvVoYEL",
		Model: "claude-3-5-sonnet-20241022",
		Content: []ContentBlock{
			{Type: "text", Text: "Hello there"},
			{
				Type:  "tool_use",
				ID:    "toolu_01",
				Name:  "get_weather",
				Input: json.RawMessage(`{"location":"Paris"}`),
			},
		},
		StopReason: "tool_use",
		Usage:      ResponseUsage{InputTokens: 100, OutputTokens: 50},
	}

	got, err := ToOpenAIResponse(resp)
	if err != nil {
		t.Fatalf("ToOpenAIResponse: %v", err)
	}

	if got.ID != resp.ID {
		t.Errorf("id: want %q got %q", resp.ID, got.ID)
	}
	if got.Model != resp.Model {
		t.Errorf("model mismatch")
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices: want 1 got %d", len(got.Choices))
	}
	ch := got.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason: want tool_calls got %q", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls: want 1 got %d", len(ch.Message.ToolCalls))
	}
	tc := ch.Message.ToolCalls[0]
	if tc.ID != "toolu_01" || tc.Function.Name != "get_weather" {
		t.Errorf("tool_call: unexpected %+v", tc)
	}
	if tc.Function.Arguments != `{"location":"Paris"}` {
		t.Errorf("arguments: want %q got %q", `{"location":"Paris"}`, tc.Function.Arguments)
	}

	if got.Usage == nil {
		t.Fatal("usage is nil")
	}
	if got.Usage.PromptTokens != 100 || got.Usage.CompletionTokens != 50 || got.Usage.TotalTokens != 150 {
		t.Errorf("usage: %+v", got.Usage)
	}
}

// TestFromOpenAIResponse is a happy-path reverse direction test.
func TestFromOpenAIResponse(t *testing.T) {
	text := "Sure thing"
	resp := &openai.ChatResponse{
		ID:    "chatcmpl-abc",
		Model: "claude-3-5-sonnet-20241022",
		Choices: []openai.Choice{
			{
				Index: 0,
				Message: openai.ChatResponseMessage{
					Role:    "assistant",
					Content: &text,
				},
				FinishReason: "stop",
			},
		},
		Usage: &openai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	got, err := FromOpenAIResponse(resp)
	if err != nil {
		t.Fatalf("FromOpenAIResponse: %v", err)
	}
	if got.ID != "chatcmpl-abc" {
		t.Errorf("id mismatch")
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason: want end_turn got %q", got.StopReason)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "Sure thing" {
		t.Errorf("content: %+v", got.Content)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Errorf("usage: %+v", got.Usage)
	}
}

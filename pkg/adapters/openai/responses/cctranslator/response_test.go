package cctranslator

import (
	"testing"

	"github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

func strPtr(s string) *string { return &s }

func TestCCToResponse_SimpleText(t *testing.T) {
	content := "Hello!"
	cc := &openai.ChatResponse{
		ID:      "chatcmpl-123",
		Created: 1700000000,
		Model:   "gpt-4o",
		Choices: []openai.Choice{
			{
				Index:        0,
				FinishReason: "stop",
				Message: openai.ChatResponseMessage{
					Role:    "assistant",
					Content: &content,
				},
			},
		},
		Usage: &openai.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}

	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.ID != "chatcmpl-123" {
		t.Errorf("id: got %q, want chatcmpl-123", resp.ID)
	}
	if resp.Object != "response" {
		t.Errorf("object: got %q, want response", resp.Object)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("model: got %q, want gpt-4o", resp.Model)
	}
	if resp.CreatedAt != 1700000000 {
		t.Errorf("created_at: got %d, want 1700000000", resp.CreatedAt)
	}
	if resp.Status != responses.StatusCompleted {
		t.Errorf("status: got %q, want completed", resp.Status)
	}
	if resp.FinishReason != responses.FinishReasonStop {
		t.Errorf("finish_reason: got %q, want stop", resp.FinishReason)
	}

	if len(resp.Output) != 1 {
		t.Fatalf("output len: got %d, want 1", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*responses.Message)
	if !ok {
		t.Fatalf("output[0] not *Message, got %T", resp.Output[0])
	}
	if msg.Role != responses.RoleAssistant {
		t.Errorf("message role: got %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: got %d, want 1", len(msg.Content))
	}
	otp, ok := msg.Content[0].(*responses.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] not *OutputTextPart, got %T", msg.Content[0])
	}
	if otp.Text != "Hello!" {
		t.Errorf("text: got %q, want Hello!", otp.Text)
	}

	// The synthesized Message item must carry id + status — spec marks both
	// as required on output_message items. id is derived from the CC id.
	if msg.ID != "msg_chatcmpl-123" {
		t.Errorf("message id: got %q, want msg_chatcmpl-123", msg.ID)
	}
	if msg.Status != responses.StatusCompleted {
		t.Errorf("message status: got %q, want completed", msg.Status)
	}

	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.TotalTokens != 15 {
		t.Errorf("usage: got %+v", resp.Usage)
	}
}

func TestCCToResponse_ModelOverride(t *testing.T) {
	content := "hi"
	cc := &openai.ChatResponse{
		ID:      "cid",
		Model:   "gpt-4o-deployment-alias",
		Choices: []openai.Choice{{FinishReason: "stop", Message: openai.ChatResponseMessage{Content: &content}}},
	}
	resp, err := CCToResponse(nil, cc, "gpt-4o")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("model override: got %q, want gpt-4o", resp.Model)
	}
}

func TestCCToResponse_WithToolCalls(t *testing.T) {
	content := ""
	cc := &openai.ChatResponse{
		ID:      "c1",
		Created: 1700000000,
		Model:   "gpt-4o",
		Choices: []openai.Choice{
			{
				FinishReason: "tool_calls",
				Message: openai.ChatResponseMessage{
					Role:    "assistant",
					Content: &content,
					ToolCalls: []openai.ToolCall{
						{
							ID:   "call_abc",
							Type: "function",
							Function: openai.ToolCallFunction{
								Name:      "get_weather",
								Arguments: `{"location":"NYC"}`,
							},
						},
						{
							ID:   "call_def",
							Type: "function",
							Function: openai.ToolCallFunction{
								Name:      "get_time",
								Arguments: `{"tz":"UTC"}`,
							},
						},
					},
				},
			},
		},
	}

	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != responses.StatusCompleted {
		t.Errorf("status: got %q", resp.Status)
	}
	if resp.FinishReason != responses.FinishReasonToolCalls {
		t.Errorf("finish_reason: got %q, want tool_calls", resp.FinishReason)
	}

	// Empty content → only tool call items (no message item).
	if len(resp.Output) != 2 {
		t.Fatalf("output len: got %d, want 2 (two function_call items)", len(resp.Output))
	}
	fc1, ok := resp.Output[0].(*responses.FunctionCall)
	if !ok {
		t.Fatalf("output[0] not *FunctionCall, got %T", resp.Output[0])
	}
	if fc1.ID != "call_abc" || fc1.Name != "get_weather" {
		t.Errorf("fc1: %+v", fc1)
	}
	if fc1.Arguments != `{"location":"NYC"}` {
		t.Errorf("fc1 args: got %q", fc1.Arguments)
	}
	fc2, ok := resp.Output[1].(*responses.FunctionCall)
	if !ok {
		t.Fatalf("output[1] not *FunctionCall, got %T", resp.Output[1])
	}
	if fc2.Name != "get_time" {
		t.Errorf("fc2 name: got %q", fc2.Name)
	}
}

func TestCCToResponse_TextAndToolCalls(t *testing.T) {
	// When there is both text content and tool calls, we expect a message item
	// followed by function_call items.
	content := "Let me check the weather."
	cc := &openai.ChatResponse{
		ID:    "c2",
		Model: "gpt-4o",
		Choices: []openai.Choice{
			{
				FinishReason: "tool_calls",
				Message: openai.ChatResponseMessage{
					Content: &content,
					ToolCalls: []openai.ToolCall{
						{ID: "call_1", Type: "function", Function: openai.ToolCallFunction{Name: "get_weather", Arguments: "{}"}},
					},
				},
			},
		},
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// message item + function_call item
	if len(resp.Output) != 2 {
		t.Fatalf("output len: got %d, want 2", len(resp.Output))
	}
	if _, ok := resp.Output[0].(*responses.Message); !ok {
		t.Errorf("output[0] should be *Message, got %T", resp.Output[0])
	}
	if _, ok := resp.Output[1].(*responses.FunctionCall); !ok {
		t.Errorf("output[1] should be *FunctionCall, got %T", resp.Output[1])
	}
}

func TestCCToResponse_FinishReasonLength(t *testing.T) {
	content := "Partial ans"
	cc := &openai.ChatResponse{
		ID:    "c3",
		Model: "gpt-4o",
		Choices: []openai.Choice{
			{FinishReason: "length", Message: openai.ChatResponseMessage{Content: &content}},
		},
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != responses.StatusIncomplete {
		t.Errorf("status: got %q, want incomplete", resp.Status)
	}
	if resp.FinishReason != responses.FinishReasonLength {
		t.Errorf("finish_reason: got %q, want length", resp.FinishReason)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("incomplete_details: got %+v", resp.IncompleteDetails)
	}
}

func TestCCToResponse_FinishReasonContentFilter(t *testing.T) {
	content := ""
	cc := &openai.ChatResponse{
		ID:    "c4",
		Model: "gpt-4o",
		Choices: []openai.Choice{
			{FinishReason: "content_filter", Message: openai.ChatResponseMessage{Content: &content}},
		},
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.FinishReason != responses.FinishReasonContentFilter {
		t.Errorf("finish_reason: got %q, want content_filter", resp.FinishReason)
	}
}

func TestCCToResponse_UsageDetails(t *testing.T) {
	content := "hi"
	cc := &openai.ChatResponse{
		ID:    "c5",
		Model: "gpt-4o",
		Choices: []openai.Choice{
			{FinishReason: "stop", Message: openai.ChatResponseMessage{Content: &content}},
		},
		Usage: &openai.Usage{
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
			PromptDetails: &openai.PromptTokenDetails{
				CachedTokens: 40,
			},
			CompletionDetails: &openai.CompletionTokenDetails{
				ReasoningTokens: 10,
			},
		},
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.InputTokens != 100 {
		t.Errorf("input_tokens: got %d, want 100", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 50 {
		t.Errorf("output_tokens: got %d, want 50", resp.Usage.OutputTokens)
	}
	if resp.Usage.TotalTokens != 150 {
		t.Errorf("total_tokens: got %d, want 150", resp.Usage.TotalTokens)
	}
	if resp.Usage.InputTokensDetails.CachedTokens != 40 {
		t.Errorf("input_tokens_details.cached_tokens: got %d, want 40", resp.Usage.InputTokensDetails.CachedTokens)
	}
	if resp.Usage.OutputTokensDetails.ReasoningTokens != 10 {
		t.Errorf("output_tokens_details.reasoning_tokens: got %d, want 10", resp.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

func TestCCToResponse_NoChoices(t *testing.T) {
	cc := &openai.ChatResponse{
		ID:    "c6",
		Model: "gpt-4o",
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No choices → completed/stop with empty output.
	if resp.Status != responses.StatusCompleted {
		t.Errorf("status: got %q", resp.Status)
	}
	if len(resp.Output) != 0 {
		t.Errorf("output len: got %d, want 0", len(resp.Output))
	}
}

func TestCCToResponse_NullContent_ToolCallsOnly(t *testing.T) {
	// CC null content with tool_calls → only function_call items (no empty message).
	cc := &openai.ChatResponse{
		ID:    "c7",
		Model: "gpt-4o",
		Choices: []openai.Choice{
			{
				FinishReason: "tool_calls",
				Message: openai.ChatResponseMessage{
					Content: nil,
					ToolCalls: []openai.ToolCall{
						{ID: "call_x", Type: "function", Function: openai.ToolCallFunction{Name: "fn", Arguments: "{}"}},
					},
				},
			},
		},
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: got %d, want 1", len(resp.Output))
	}
	if _, ok := resp.Output[0].(*responses.FunctionCall); !ok {
		t.Errorf("output[0] should be *FunctionCall, got %T", resp.Output[0])
	}
}

func TestCCToResponse_Refusal(t *testing.T) {
	refusal := "I cannot help with that."
	cc := &openai.ChatResponse{
		ID:    "c8",
		Model: "gpt-4o",
		Choices: []openai.Choice{
			{
				FinishReason: "stop",
				Message: openai.ChatResponseMessage{
					Content: nil,
					Refusal: &refusal,
				},
			},
		},
	}
	resp, err := CCToResponse(nil, cc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: got %d, want 1", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*responses.Message)
	if !ok {
		t.Fatalf("output[0] not *Message, got %T", resp.Output[0])
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: got %d", len(msg.Content))
	}
	rp, ok := msg.Content[0].(*responses.RefusalPart)
	if !ok {
		t.Fatalf("content[0] not *RefusalPart, got %T", msg.Content[0])
	}
	if rp.Refusal != refusal {
		t.Errorf("refusal text: got %q", rp.Refusal)
	}
}

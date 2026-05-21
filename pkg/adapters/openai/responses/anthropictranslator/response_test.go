package anthropictranslator

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

func TestAnthropicToResponse_TextBlock(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_01",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-opus-4-5",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello, world!"},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.ID != "msg_01" {
		t.Errorf("id: got %q", resp.ID)
	}
	if resp.Object != "response" {
		t.Errorf("object: got %q", resp.Object)
	}
	if resp.Model != "claude-opus-4-5" {
		t.Errorf("model: got %q", resp.Model)
	}
	if resp.Status != responses.StatusCompleted {
		t.Errorf("status: got %q", resp.Status)
	}
	if resp.FinishReason != responses.FinishReasonStop {
		t.Errorf("finish_reason: got %q", resp.FinishReason)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: got %d want 1", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*responses.Message)
	if !ok {
		t.Fatalf("output[0] is %T, want *responses.Message", resp.Output[0])
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: got %d", len(msg.Content))
	}
	part, ok := msg.Content[0].(*responses.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if part.Text != "Hello, world!" {
		t.Errorf("text: got %q", part.Text)
	}

	// Spec marks id + status as required on output_message items.
	if msg.ID != "msg_0" {
		t.Errorf("message id: got %q, want msg_0", msg.ID)
	}
	if msg.Status != responses.StatusCompleted {
		t.Errorf("message status: got %q, want completed", msg.Status)
	}

	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("input_tokens: got %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("output_tokens: got %d", resp.Usage.OutputTokens)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens: got %d", resp.Usage.TotalTokens)
	}
}

func TestAnthropicToResponse_ToolUseBlock(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_02",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_01",
				"name":  "search",
				"input": map[string]any{"q": "golang"},
			},
		},
		"stop_reason": "tool_use",
		"usage": map[string]any{
			"input_tokens":  20,
			"output_tokens": 15,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Status != responses.StatusCompleted {
		t.Errorf("status: got %q", resp.Status)
	}
	if resp.FinishReason != responses.FinishReasonToolCalls {
		t.Errorf("finish_reason: got %q", resp.FinishReason)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: got %d", len(resp.Output))
	}
	fc, ok := resp.Output[0].(*responses.FunctionCall)
	if !ok {
		t.Fatalf("output[0] is %T, want *responses.FunctionCall", resp.Output[0])
	}
	if fc.CallID != "toolu_01" {
		t.Errorf("call_id: got %q", fc.CallID)
	}
	if fc.Name != "search" {
		t.Errorf("name: got %q", fc.Name)
	}
	// arguments should be the JSON-encoded input
	var args map[string]any
	if err := json.Unmarshal([]byte(fc.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["q"] != "golang" {
		t.Errorf("arguments.q: got %q", args["q"])
	}
}

func TestAnthropicToResponse_ThinkingBlock(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_03",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-opus-4-5",
		"content": []any{
			map[string]any{
				"type":     "thinking",
				"thinking": "Let me reason through this step by step...",
			},
			map[string]any{"type": "text", "text": "The answer is 42."},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  50,
			"output_tokens": 30,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Output) != 2 {
		t.Fatalf("output len: got %d want 2", len(resp.Output))
	}

	// First item should be reasoning.
	reasoning, ok := resp.Output[0].(*responses.Reasoning)
	if !ok {
		t.Fatalf("output[0] is %T, want *responses.Reasoning", resp.Output[0])
	}
	if len(reasoning.Summary) == 0 || reasoning.Summary[0].Text == "" {
		t.Error("reasoning summary should contain thinking text")
	}

	// Second item should be message.
	msg, ok := resp.Output[1].(*responses.Message)
	if !ok {
		t.Fatalf("output[1] is %T, want *responses.Message", resp.Output[1])
	}
	if len(msg.Content) == 0 {
		t.Fatal("message content is empty")
	}
}

func TestAnthropicToResponse_StopReasonVariants(t *testing.T) {
	cases := []struct {
		stopReason     string
		wantStatus     responses.Status
		wantFinish     responses.FinishReason
		wantIncomplete string
	}{
		{"end_turn", responses.StatusCompleted, responses.FinishReasonStop, ""},
		{"stop_sequence", responses.StatusCompleted, responses.FinishReasonStop, ""},
		{"max_tokens", responses.StatusIncomplete, responses.FinishReasonLength, "max_output_tokens"},
		{"tool_use", responses.StatusCompleted, responses.FinishReasonToolCalls, ""},
		{"refusal", responses.StatusCompleted, "refusal", ""},
		{"pause_turn", responses.StatusIncomplete, "", "pause_turn"},
		{"", responses.StatusCompleted, responses.FinishReasonStop, ""},
		{"unknown_future", responses.StatusCompleted, responses.FinishReasonStop, ""},
	}

	for _, tc := range cases {
		t.Run(tc.stopReason, func(t *testing.T) {
			body := mustJSON(map[string]any{
				"id":          "msg_test",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-opus-4-5",
				"content":     []any{map[string]any{"type": "text", "text": "ok"}},
				"stop_reason": tc.stopReason,
				"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
			})
			resp, err := AnthropicToResponse(nil, body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("status: got %q want %q", resp.Status, tc.wantStatus)
			}
			if resp.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason: got %q want %q", resp.FinishReason, tc.wantFinish)
			}
			if tc.wantIncomplete != "" {
				if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != tc.wantIncomplete {
					t.Errorf("incomplete_details.reason: got %v want %q", resp.IncompleteDetails, tc.wantIncomplete)
				}
			} else {
				if resp.IncompleteDetails != nil {
					t.Errorf("incomplete_details should be nil, got %v", resp.IncompleteDetails)
				}
			}
		})
	}
}

func TestAnthropicToResponse_UsageWithCachedTokens(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_04",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-opus-4-5",
		"content": []any{
			map[string]any{"type": "text", "text": "Cached response."},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":              100,
			"output_tokens":             50,
			"cache_read_input_tokens":   80,
			"cache_creation_input_tokens": 20,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.InputTokensDetails.CachedTokens != 80 {
		t.Errorf("cached_tokens: got %d want 80", resp.Usage.InputTokensDetails.CachedTokens)
	}
}

func TestAnthropicToResponse_Refusal(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_refusal",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-opus-4-5",
		"content": []any{
			map[string]any{"type": "text", "text": "I cannot help with that request."},
		},
		"stop_reason": "refusal",
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 8,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Status != responses.StatusCompleted {
		t.Errorf("status: got %q", resp.Status)
	}
	if resp.FinishReason != "refusal" {
		t.Errorf("finish_reason: got %q want refusal", resp.FinishReason)
	}
	// Text content (refusal message) should be preserved.
	if len(resp.Output) == 0 {
		t.Fatal("output should not be empty for refusal")
	}
	msg, ok := resp.Output[0].(*responses.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	part, ok := msg.Content[0].(*responses.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if part.Text != "I cannot help with that request." {
		t.Errorf("refusal text: got %q", part.Text)
	}
}

func TestAnthropicToResponse_URLCitationAnnotations(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_cit",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-opus-4-5",
		"content": []any{
			map[string]any{
				"type": "text",
				"text": "According to the docs...",
				"citations": []any{
					map[string]any{
						"type":        "url_citation",
						"url":         "https://docs.anthropic.com",
						"title":       "Anthropic Docs",
						"start_index": 0,
						"end_index":   5,
					},
					map[string]any{
						"type":        "char_location",
						"document_id": "doc_1",
						"start_char":  10,
						"end_char":    20,
					},
				},
			},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg, ok := resp.Output[0].(*responses.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	part, ok := msg.Content[0].(*responses.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	// Only url_citation should be mapped; char_location is dropped.
	if len(part.Annotations) != 1 {
		t.Errorf("annotations len: got %d want 1 (char_location should be dropped)", len(part.Annotations))
	}
	cit, ok := part.Annotations[0].(*responses.URLCitationAnnotation)
	if !ok {
		t.Fatalf("annotation[0] is %T", part.Annotations[0])
	}
	if cit.URL != "https://docs.anthropic.com" {
		t.Errorf("url: got %q", cit.URL)
	}
}

func TestAnthropicToResponse_RedactedThinkingDropped(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_redact",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-opus-4-5",
		"content": []any{
			map[string]any{"type": "redacted_thinking", "data": "REDACTED"},
			map[string]any{"type": "text", "text": "Here is my answer."},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	})

	resp, err := AnthropicToResponse(nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// redacted_thinking is dropped; only the text block remains.
	if len(resp.Output) != 1 {
		t.Errorf("output len: got %d want 1 (redacted_thinking should be dropped)", len(resp.Output))
	}
}

// mustJSON encodes v to JSON or panics.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

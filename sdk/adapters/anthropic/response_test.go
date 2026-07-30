package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseResponse tests ----

func TestAnthropicParseResponse_SimpleText(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":          "msg_abc",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-3-5-sonnet-20241022",
		"content":     []any{map[string]any{"type": "text", "text": "Hello!"}},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "msg_abc" {
		t.Errorf("id: %q", resp.ID)
	}
	if resp.Status != v1.StatusCompleted {
		t.Errorf("status: %q", resp.Status)
	}
	if resp.FinishReason != v1.FinishReasonStop {
		t.Errorf("finish_reason: %q", resp.FinishReason)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	tp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if tp.Text != "Hello!" {
		t.Errorf("text: %q", tp.Text)
	}
	if resp.Usage["input"] != 10 || resp.Usage["output"] != 5 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestAnthropicParseResponse_ToolUse(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_tool",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_01",
				"name":  "search",
				"input": map[string]any{"q": "something"},
			},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonToolCalls {
		t.Errorf("finish_reason: %q", resp.FinishReason)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	fc, ok := resp.Output[0].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	if fc.CallID != "toolu_01" || fc.Name != "search" {
		t.Errorf("fc: callID=%q name=%q", fc.CallID, fc.Name)
	}
}

func TestAnthropicParseResponse_ThinkingBlock(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_think",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-7-sonnet-20250219",
		"content": []any{
			map[string]any{
				"type":      "thinking",
				"thinking":  "let me think...",
				"signature": "sig_abc123",
			},
			map[string]any{"type": "text", "text": "Answer."},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 20},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	reasoning, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	if reasoning.Content != "let me think..." {
		t.Errorf("reasoning content: %q", reasoning.Content)
	}
	// ProviderData should carry the signature.
	if len(reasoning.ProviderData) == 0 {
		t.Error("expected ProviderData for thinking signature")
	}
	var pd map[string]string
	_ = json.Unmarshal(reasoning.ProviderData, &pd)
	if pd["signature"] != "sig_abc123" {
		t.Errorf("signature in ProviderData: %q", pd["signature"])
	}
}

func TestAnthropicParseResponse_MaxTokens(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":          "msg_len",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-3-5-sonnet-20241022",
		"content":     []any{map[string]any{"type": "text", "text": "truncated"}},
		"stop_reason": "max_tokens",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 100},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete {
		t.Errorf("status: %q", resp.Status)
	}
	if resp.FinishReason != v1.FinishReasonLength {
		t.Errorf("finish_reason: %q", resp.FinishReason)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("incomplete_details: %v", resp.IncompleteDetails)
	}
}

func TestAnthropicParseResponse_PauseTurn(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":          "msg_pause",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-3-5-sonnet-20241022",
		"content":     []any{map[string]any{"type": "text", "text": "..."}},
		"stop_reason": "pause_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 5},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete {
		t.Errorf("status: %q", resp.Status)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "pause_turn" {
		t.Errorf("incomplete_details: %v", resp.IncompleteDetails)
	}
}

func TestAnthropicParseResponse_Refusal(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":          "msg_ref",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-3-5-sonnet-20241022",
		"content":     []any{map[string]any{"type": "text", "text": "I cannot do that."}},
		"stop_reason": "refusal",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 5},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonRefusal {
		t.Errorf("finish_reason: %q", resp.FinishReason)
	}
}

func TestAnthropicParseResponse_CachedTokens(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":          "msg_cache",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-3-5-sonnet-20241022",
		"content":     []any{map[string]any{"type": "text", "text": "ok"}},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":            50,
			"output_tokens":           10,
			"cache_read_input_tokens": 30,
		},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Usage) == 0 {
		t.Fatal("no usage")
	}
	if resp.Usage["cache_read"] != 30 {
		t.Errorf("cache_read: %d", resp.Usage["cache_read"])
	}
}

// ---- SerializeResponse tests ----

func TestAnthropicSerializeResponse_SimpleText(t *testing.T) {
	resp := &v1.Response{
		ID:           "msg_abc",
		Model:        "claude-3-5-sonnet-20241022",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{
			&v1.Message{
				ID:      "msg_0",
				Role:    v1.RoleAssistant,
				Status:  v1.StatusCompleted,
				Content: []v1.Part{&v1.OutputTextPart{Text: "Hello!"}},
			},
		},
		Usage: usage.Tokens{"input": 10, "output": 5},
	}
	out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["id"] != "msg_abc" {
		t.Errorf("id: %v", m["id"])
	}
	if m["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
	content := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content len: %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello!" {
		t.Errorf("content block: %v", block)
	}
}

func TestAnthropicSerializeResponse_ToolUse(t *testing.T) {
	resp := &v1.Response{
		ID:           "msg_tool",
		Model:        "claude-3-5-sonnet-20241022",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonToolCalls,
		Output: []v1.Item{
			&v1.FunctionCall{
				ID:        "fc_0",
				CallID:    "toolu_01",
				Name:      "search",
				Arguments: `{"q":"something"}`,
				Status:    v1.StatusCompleted,
			},
		},
		Usage: usage.Tokens{"input": 5, "output": 3},
	}
	out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
	content := m["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" {
		t.Errorf("block type: %v", block["type"])
	}
	if block["id"] != "toolu_01" || block["name"] != "search" {
		t.Errorf("tool_use: %v", block)
	}
}

func TestAnthropicSerializeResponse_ThinkingBlock(t *testing.T) {
	pd, _ := json.Marshal(map[string]string{
		"type":      "thinking",
		"thinking":  "my thoughts",
		"signature": "sig_xyz",
	})
	resp := &v1.Response{
		ID:           "msg_think",
		Model:        "claude-3-7-sonnet-20250219",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{
			&v1.Reasoning{
				ID:           "rs_0",
				Content:      "my thoughts",
				ProviderData: pd,
				Status:       v1.StatusCompleted,
			},
			&v1.Message{
				ID:      "msg_0",
				Role:    v1.RoleAssistant,
				Status:  v1.StatusCompleted,
				Content: []v1.Part{&v1.OutputTextPart{Text: "Answer."}},
			},
		},
		Usage: usage.Tokens{"input": 5, "output": 20},
	}
	out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	content := m["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len: %d", len(content))
	}
	thinkBlock := content[0].(map[string]any)
	if thinkBlock["type"] != "thinking" {
		t.Errorf("block type: %v", thinkBlock["type"])
	}
	if thinkBlock["thinking"] != "my thoughts" {
		t.Errorf("thinking: %v", thinkBlock["thinking"])
	}
	if thinkBlock["signature"] != "sig_xyz" {
		t.Errorf("signature: %v", thinkBlock["signature"])
	}
}

func TestAnthropicSerializeResponse_ReqParamIsNilSafe(t *testing.T) {
	resp := &v1.Response{
		ID:           "msg_nil",
		Model:        "claude-3-5-sonnet-20241022",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{
			&v1.Message{
				Role:    v1.RoleAssistant,
				Content: []v1.Part{&v1.OutputTextPart{Text: "ok"}},
			},
		},
	}
	// req=nil must not panic
	out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Error("empty output")
	}
}

func TestAnthropicSerializeResponse_MaxTokens(t *testing.T) {
	resp := &v1.Response{
		ID:                "msg_len",
		Model:             "claude-3-5-sonnet-20241022",
		Status:            v1.StatusIncomplete,
		FinishReason:      v1.FinishReasonLength,
		IncompleteDetails: &v1.IncompleteDetails{Reason: "max_output_tokens"},
		Output: []v1.Item{
			&v1.Message{
				Role:    v1.RoleAssistant,
				Content: []v1.Part{&v1.OutputTextPart{Text: "truncated"}},
			},
		},
		Usage: usage.Tokens{"input": 10, "output": 100},
	}
	out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
}

// ---- A-3 regression: stop_sequence in Extensions (fix: ParseResponse surfaces it) ----

// TestParseResponse_StopSequenceInExtensions verifies that a matched stop_sequence
// is surfaced in Response.Extensions["stop_sequence"].
func TestParseResponse_StopSequenceInExtensions(t *testing.T) {
	body := mustJSON(map[string]any{
		"id": "msg_ss", "type": "message", "role": "assistant",
		"model":         "claude-3-5-sonnet-20241022",
		"content":       []any{map[string]any{"type": "text", "text": "done"}},
		"stop_reason":   "stop_sequence",
		"stop_sequence": "END",
		"usage":         map[string]any{"input_tokens": 5, "output_tokens": 3},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := resp.Extensions["stop_sequence"]
	if !ok {
		t.Fatal("Extensions[stop_sequence] absent")
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal stop_sequence: %v", err)
	}
	if got != "END" {
		t.Errorf("stop_sequence: got %q want END", got)
	}
}

// TestParseResponse_NoStopSequence_NoExtensions verifies that absent stop_sequence
// does not pollute Extensions.
func TestParseResponse_NoStopSequence_NoExtensions(t *testing.T) {
	body := mustJSON(map[string]any{
		"id": "msg_noss", "type": "message", "role": "assistant",
		"model":       "claude-3-5-sonnet-20241022",
		"content":     []any{map[string]any{"type": "text", "text": "done"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Extensions["stop_sequence"]; ok {
		t.Error("Extensions[stop_sequence] present but stop_sequence was absent in response")
	}
}

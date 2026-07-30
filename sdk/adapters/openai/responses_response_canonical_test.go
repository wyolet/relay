package openai

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestResponsesParseResponse_SimpleText(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_01",
		"object":     "response",
		"created_at": int64(1700000000),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_0",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "Hello!",
			}},
		}},
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
			"total_tokens":  15,
		},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_01" {
		t.Errorf("id: %q", resp.ID)
	}
	if resp.Status != v1.StatusCompleted {
		t.Errorf("status: %q", resp.Status)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	if resp.Usage["input"] != 10 {
		t.Errorf("usage: %v", resp.Usage)
	}
}

// TestResponsesParseResponse_FinishReasonFromStatus locks the fix for the
// fabricated finish_reason: the Responses API has no finish_reason field, so the
// canonical finish_reason must be derived from status + incomplete_details.reason.
// Before the fix, every non-stop terminal masqueraded as a clean stop.
func TestResponsesParseResponse_FinishReasonFromStatus(t *testing.T) {
	mk := func(status, incompleteReason string, withFnCall bool) []byte {
		m := map[string]any{
			"id": "resp_fr", "object": "response", "model": "gpt-5", "status": status,
			"output": []any{},
		}
		if incompleteReason != "" {
			m["incomplete_details"] = map[string]any{"reason": incompleteReason}
		}
		if withFnCall {
			m["output"] = []any{map[string]any{
				"type": "function_call", "id": "fc_0", "call_id": "c0",
				"name": "f", "arguments": "{}",
			}}
		}
		return mustJSON(m)
	}
	cases := []struct {
		name, status, reason string
		fnCall               bool
		want                 v1.FinishReason
	}{
		{"plain stop", "completed", "", false, v1.FinishReasonStop},
		{"tool calls", "completed", "", true, v1.FinishReasonToolCalls},
		{"truncated", "incomplete", "max_output_tokens", false, v1.FinishReasonLength},
		{"content filter", "incomplete", "content_filter", false, v1.FinishReasonContentFilter},
		{"incomplete unknown reason", "incomplete", "", false, v1.FinishReasonLength},
		{"failed has no finish_reason", "failed", "", false, v1.FinishReason("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := (ResponsesTranslator{}).ParseResponse(mk(tc.status, tc.reason, tc.fnCall))
			if err != nil {
				t.Fatal(err)
			}
			if resp.FinishReason != tc.want {
				t.Errorf("finish_reason: got %q, want %q", resp.FinishReason, tc.want)
			}
			if resp.Status != v1.Status(tc.status) {
				t.Errorf("status: got %q, want %q", resp.Status, tc.status)
			}
		})
	}
}

// TestResponsesSerializeResponse_StatusFromFinishReason locks the inverse: a
// canonical length/content_filter finish_reason must serialize to a Responses
// body as status=incomplete + incomplete_details.reason, with NO finish_reason field.
func TestResponsesSerializeResponse_StatusFromFinishReason(t *testing.T) {
	cases := []struct {
		name       string
		fr         v1.FinishReason
		status     v1.Status
		wantStatus string
		wantReason string
	}{
		{"length", v1.FinishReasonLength, v1.StatusIncomplete, "incomplete", "max_output_tokens"},
		{"content_filter", v1.FinishReasonContentFilter, v1.StatusCompleted, "incomplete", "content_filter"},
		{"stop", v1.FinishReasonStop, v1.StatusCompleted, "completed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := (ResponsesTranslator{}).SerializeResponse(
				&v1.Response{ID: "r", Model: "gpt-5", Status: tc.status, FinishReason: tc.fr}, nil)
			if err != nil {
				t.Fatal(err)
			}
			m := decodeMap(t, b)
			if _, ok := m["finish_reason"]; ok {
				t.Errorf("finish_reason must not appear: %v", m["finish_reason"])
			}
			if m["status"] != tc.wantStatus {
				t.Errorf("status: got %v, want %q", m["status"], tc.wantStatus)
			}
			if tc.wantReason != "" {
				inc, ok := m["incomplete_details"].(map[string]any)
				if !ok || inc["reason"] != tc.wantReason {
					t.Errorf("incomplete_details.reason: got %v, want %q", m["incomplete_details"], tc.wantReason)
				}
			}
		})
	}
}

func TestResponsesParseResponse_ToolCall(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_02",
		"object":     "response",
		"created_at": int64(1700000001),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":      "function_call",
			"id":        "fc_01",
			"call_id":   "call_abc",
			"name":      "search",
			"arguments": `{"q":"golang"}`,
			"status":    "completed",
		}},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var foundFC bool
	for _, item := range resp.Output {
		if fc, ok := item.(*v1.FunctionCall); ok {
			if fc.CallID != "call_abc" {
				t.Errorf("call_id: %q", fc.CallID)
			}
			if fc.Name != "search" {
				t.Errorf("name: %q", fc.Name)
			}
			foundFC = true
		}
	}
	if !foundFC {
		t.Error("expected FunctionCall in output")
	}
}

func TestResponsesParseResponse_ReasoningItem(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_03",
		"object":     "response",
		"created_at": int64(1700000002),
		"model":      "o3",
		"status":     "completed",
		"output": []any{
			map[string]any{
				"type":   "reasoning",
				"id":     "rs_0",
				"status": "completed",
				"summary": []any{map[string]any{
					"type": "summary_text",
					"text": "Let me think step by step.",
				}},
			},
			map[string]any{
				"type":   "message",
				"id":     "msg_0",
				"status": "completed",
				"role":   "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": "The answer is 42.",
				}},
			},
		},
		"usage": map[string]any{"input_tokens": 50, "output_tokens": 30, "total_tokens": 80},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	if _, ok := resp.Output[0].(*v1.Reasoning); !ok {
		t.Errorf("output[0] is %T, want *v1.Reasoning", resp.Output[0])
	}
	if _, ok := resp.Output[1].(*v1.Message); !ok {
		t.Errorf("output[1] is %T, want *v1.Message", resp.Output[1])
	}
}

func TestResponsesParseResponse_Refusal(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_ref",
		"object":     "response",
		"created_at": int64(1700000003),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_0",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":    "refusal",
				"refusal": "I cannot help with that.",
			}},
		}},
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 8, "total_tokens": 13},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) == 0 {
		t.Fatal("expected output items")
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	// Refusal part maps to OutputTextPart (canonical rule 9).
	if len(msg.Content) == 0 {
		t.Error("expected refusal text in content")
	}
	tp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if tp.Text != "I cannot help with that." {
		t.Errorf("refusal text: %q", tp.Text)
	}
}

func TestResponsesParseResponse_UsageDetails(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_u",
		"object":     "response",
		"created_at": int64(1700000000),
		"model":      "gpt-5",
		"status":     "completed",
		"output":     []any{},
		"usage": map[string]any{
			"input_tokens":          200,
			"output_tokens":         100,
			"total_tokens":          300,
			"input_tokens_details":  map[string]any{"cached_tokens": 150},
			"output_tokens_details": map[string]any{"reasoning_tokens": 40},
		},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Usage) == 0 {
		t.Fatal("usage is nil")
	}
	// Responses input_tokens=200 includes cached=150; canonical "input"
	// is non-cached only. Sum gives 200 back.
	if resp.Usage["input"] != 50 {
		t.Errorf("non-cached input: %d", resp.Usage["input"])
	}
	if resp.Usage["cache_read"] != 150 {
		t.Errorf("cache_read: %d", resp.Usage["cache_read"])
	}
	if resp.Usage["reasoning"] != 40 {
		t.Errorf("reasoning: %d", resp.Usage["reasoning"])
	}
}

// --- SerializeResponse ---

func TestResponsesSerializeResponse_SimpleText(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_01",
		Model:        "gpt-5",
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
	req := &v1.Request{Model: v1.ModelRefs{"gpt-5"}}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["object"] != "response" {
		t.Errorf("object: %v", m["object"])
	}
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	output, ok := m["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output: %v", m["output"])
	}
}

func TestResponsesSerializeResponse_RequestEchoFields(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_echo",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
	}
	req := &v1.Request{
		Model:        v1.ModelRefs{"gpt-5"},
		Instructions: "be helpful",
		User:         "user-123",
		Metadata:     map[string]string{"k": "v"},
		ModelConfig: map[string]*v1.ModelOpts{
			"gpt-5": {
				Sampling: &v1.SamplingParams{
					Temperature: floatPtr(0.7),
				},
			},
		},
	}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	// Spec requires echo fields on the response.
	if m["instructions"] != "be helpful" {
		t.Errorf("instructions: %v", m["instructions"])
	}
}

func TestResponsesSerializeResponse_NilRequestAllowed(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_nil",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
	}
	_, err := (ResponsesTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatalf("unexpected error with nil req: %v", err)
	}
}

func TestResponsesSerializeResponse_IncompleteStatus(t *testing.T) {
	resp := &v1.Response{
		ID:                "resp_inc",
		Model:             "gpt-5",
		Status:            v1.StatusIncomplete,
		FinishReason:      v1.FinishReasonLength,
		IncompleteDetails: &v1.IncompleteDetails{Reason: "max_output_tokens"},
	}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["status"] != "incomplete" {
		t.Errorf("status: %v", m["status"])
	}
}

// --- ParseResponse / SerializeResponse round-trip ---

func TestResponsesSerializeResponse_ToolCallOutput(t *testing.T) {
	resp := &v1.Response{
		ID:           "resp_fc",
		Model:        "gpt-5",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonToolCalls,
		Output: []v1.Item{
			&v1.FunctionCall{
				ID:        "fc_0",
				CallID:    "call_abc",
				Name:      "search",
				Arguments: `{"q":"golang"}`,
				Status:    v1.StatusCompleted,
			},
		},
	}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	// Responses has no finish_reason field; tool_calls is conveyed by
	// status=completed + a function_call output item.
	if _, ok := m["finish_reason"]; ok {
		t.Errorf("finish_reason must not appear on a Responses body: %v", m["finish_reason"])
	}
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	output := m["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len: %d", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "function_call" {
		t.Errorf("item type: %v", item["type"])
	}
}

func TestResponsesRoundTrip_Response(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_rt",
		"object":     "response",
		"created_at": int64(1700000000),
		"model":      "gpt-5",
		"status":     "completed",
		"output": []any{map[string]any{
			"type":   "message",
			"id":     "msg_0",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "Round trip.",
			}},
		}},
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
	})

	tr := ResponsesTranslator{}
	req := &v1.Request{Model: v1.ModelRefs{"gpt-5"}}
	resp, err := tr.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := tr.SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b2)
	if m["status"] != "completed" {
		t.Errorf("status: %v", m["status"])
	}
	output := m["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len: %d", len(output))
	}
}

// --- NewToCanonicalStream (Responses → canonical) ---

// R-1: encrypted_content round-trips through ProviderData.
func TestResponsesParseResponse_EncryptedContentRoundTrip(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":            "resp_enc",
		"object":        "response",
		"created_at":    1000,
		"model":         "o1",
		"status":        "completed",
		"finish_reason": "stop",
		"output": []any{map[string]any{
			"type":              "reasoning",
			"id":                "rs_abc",
			"status":            "completed",
			"encrypted_content": "ENCBLOB",
			"summary":           []any{map[string]any{"text": "the answer is 42"}},
		}},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	if len(r.ProviderData) == 0 {
		t.Fatal("ProviderData must not be empty when encrypted_content is present")
	}
	var pd struct {
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(r.ProviderData, &pd); err != nil {
		t.Fatalf("ProviderData unmarshal: %v", err)
	}
	if pd.EncryptedContent != "ENCBLOB" {
		t.Errorf("encrypted_content: %q", pd.EncryptedContent)
	}

	// Serialize back and confirm encrypted_content is restored in the wire body.
	req := &v1.Request{Model: v1.ModelRefs{"o1"}}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	var wireResp struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(b, &wireResp); err != nil {
		t.Fatal(err)
	}
	if len(wireResp.Output) != 1 {
		t.Fatalf("serialized output len: %d", len(wireResp.Output))
	}
	var wireItem struct {
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(wireResp.Output[0], &wireItem); err != nil {
		t.Fatal(err)
	}
	if wireItem.EncryptedContent != "ENCBLOB" {
		t.Errorf("round-tripped encrypted_content: %q", wireItem.EncryptedContent)
	}
}

// TestResponsesParseResponse_HostedToolItemNoError locks the don't-500 fix: a
// response carrying a hosted-tool output item (web_search_call) must parse
// successfully — the item is dropped from canonical (no representation) but the
// surrounding message survives. Before the fix the whole ParseResponse errored.
func TestResponsesParseResponse_HostedToolItemNoError(t *testing.T) {
	body := mustJSON(map[string]any{
		"id": "resp_ws", "object": "response", "model": "gpt-5", "status": "completed",
		"output": []any{
			map[string]any{
				"type": "web_search_call", "id": "ws_0", "status": "completed",
				"action": map[string]any{"type": "search", "query": "golang"},
			},
			map[string]any{
				"type": "message", "id": "msg_0", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "Found it."}},
			},
		},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatalf("hosted-tool item must not error the parse: %v", err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d (web_search_call should drop, message should survive)", len(resp.Output))
	}
	if _, ok := resp.Output[0].(*v1.Message); !ok {
		t.Fatalf("surviving item is %T, want *v1.Message", resp.Output[0])
	}
}

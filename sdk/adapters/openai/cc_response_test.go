package openai

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestCCParseResponse_SimpleText(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":      "chatcmpl-01",
		"object":  "chat.completion",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Hello!"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "chatcmpl-01" {
		t.Errorf("id: %q", resp.ID)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("model: %q", resp.Model)
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
	if len(msg.Content) != 1 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	if resp.Usage["input"] != 10 {
		t.Errorf("usage: %v", resp.Usage)
	}
}

func TestCCParseResponse_ToolCall(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":      "chatcmpl-02",
		"object":  "chat.completion",
		"created": int64(1700000001),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   "call_abc",
					"type": "function",
					"function": map[string]any{
						"name":      "search",
						"arguments": `{"q":"golang"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 15, "total_tokens": 35},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonToolCalls {
		t.Errorf("finish_reason: %q", resp.FinishReason)
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

func TestCCParseResponse_Refusal(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":      "chatcmpl-ref",
		"object":  "chat.completion",
		"created": int64(1700000002),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"refusal": "I cannot help with that.",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 8, "total_tokens": 13},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
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
	// Refusal text should appear as message content (canonical rule 9).
	if len(msg.Content) == 0 {
		t.Error("expected refusal text in content")
	}
}

func TestCCParseResponse_FinishReasonMappings(t *testing.T) {
	cases := []struct {
		reason         string
		wantStatus     v1.Status
		wantFinish     v1.FinishReason
		wantIncomplete bool
	}{
		{"stop", v1.StatusCompleted, v1.FinishReasonStop, false},
		{"length", v1.StatusIncomplete, v1.FinishReasonLength, true},
		{"tool_calls", v1.StatusCompleted, v1.FinishReasonToolCalls, false},
		{"content_filter", v1.StatusCompleted, v1.FinishReasonContentFilter, false},
		{"unknown_future", v1.StatusCompleted, v1.FinishReasonStop, false},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			body := mustJSON(map[string]any{
				"id":      "cid",
				"object":  "chat.completion",
				"created": int64(1700000000),
				"model":   "gpt-4o",
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": "ok"},
					"finish_reason": tc.reason,
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
			resp, err := (CCTranslator{}).ParseResponse(body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("status: got %q want %q", resp.Status, tc.wantStatus)
			}
			if resp.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason: got %q want %q", resp.FinishReason, tc.wantFinish)
			}
			if tc.wantIncomplete && resp.IncompleteDetails == nil {
				t.Error("expected incomplete_details")
			}
		})
	}
}

func TestCCParseResponse_UsageDetails(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":      "cid",
		"object":  "chat.completion",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     100,
			"completion_tokens": 50,
			"total_tokens":      150,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": 80,
			},
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": 20,
			},
		},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Usage) == 0 {
		t.Fatal("usage is nil")
	}
	// OpenAI prompt_tokens=100 includes cached=80; canonical "input" is
	// non-cached only (orthogonal-meter semantics). Sum gives 100 back.
	if resp.Usage["input"] != 20 {
		t.Errorf("non-cached input: %d", resp.Usage["input"])
	}
	if resp.Usage["cache_read"] != 80 {
		t.Errorf("cache_read: %d", resp.Usage["cache_read"])
	}
	if resp.Usage["reasoning"] != 20 {
		t.Errorf("reasoning: %d", resp.Usage["reasoning"])
	}
}

// --- SerializeResponse ---

func TestCCSerializeResponse_SimpleText(t *testing.T) {
	resp := &v1.Response{
		ID:           "chatcmpl-01",
		Model:        "gpt-4o",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{
			&v1.Message{
				Role:    v1.RoleAssistant,
				Content: []v1.Part{&v1.OutputTextPart{Text: "Hello!"}},
			},
		},
		Usage: usage.Tokens{"input": 10, "output": 5},
	}
	b, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["object"] != "chat.completion" {
		t.Errorf("object: %v", m["object"])
	}
	choices := m["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices len: %d", len(choices))
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason: %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello!" {
		t.Errorf("content: %v", msg["content"])
	}
}

func TestCCSerializeResponse_NilRequestAllowed(t *testing.T) {
	resp := &v1.Response{
		ID:           "cid",
		Model:        "gpt-4o",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonStop,
	}
	// req=nil must not panic (CC doesn't need echo fields)
	_, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatalf("unexpected error with nil req: %v", err)
	}
}

func TestCCSerializeResponse_ToolCalls(t *testing.T) {
	resp := &v1.Response{
		ID:           "cid",
		Model:        "gpt-4o",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonToolCalls,
		Output: []v1.Item{
			&v1.FunctionCall{
				CallID:    "call_abc",
				Name:      "search",
				Arguments: `{"q":"golang"}`,
				Status:    v1.StatusCompleted,
			},
		},
	}
	b, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	choices := m["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	toolCalls, ok := msg["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("tool_calls: %v", msg["tool_calls"])
	}
	tc := toolCalls[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("function name: %v", fn["name"])
	}
}

func TestCCSerializeResponse_Refusal(t *testing.T) {
	resp := &v1.Response{
		ID:           "cid",
		Model:        "gpt-4o",
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonRefusal,
		Output: []v1.Item{
			&v1.Message{
				Role:    v1.RoleAssistant,
				Content: []v1.Part{&v1.OutputTextPart{Text: "I cannot help."}},
			},
		},
	}
	b, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	choices := m["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	// Refusal maps to message.refusal field in CC.
	if msg["refusal"] != "I cannot help." {
		t.Errorf("refusal: %v (msg=%v)", msg["refusal"], msg)
	}
}

// --- ParseResponse / SerializeResponse round-trip ---

func TestCCRoundTrip_Response(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":      "chatcmpl-rt",
		"object":  "chat.completion",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Round trip."},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})

	tr := CCTranslator{}
	resp, err := tr.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b2)
	choices := m["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Round trip." {
		t.Errorf("content: %v", msg["content"])
	}
}

// --- NewToCanonicalStream ---

// CC-2: URL-citation annotations are mapped to OutputTextPart.Annotations.
func TestCCParseResponse_URLCitationAnnotation(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":      "chatcmpl-ann",
		"object":  "chat.completion",
		"created": 1000,
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "Paris is the capital [1].",
				"annotations": []any{map[string]any{
					"type": "url_citation",
					"url_citation": map[string]any{
						"start_index": 21,
						"end_index":   24,
						"url":         "https://example.com/paris",
						"title":       "Paris",
					},
				}},
			},
			"finish_reason": "stop",
		}},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	part, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if len(part.Annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(part.Annotations))
	}
	ann, ok := part.Annotations[0].(*v1.URLCitationAnnotation)
	if !ok {
		t.Fatalf("annotation is %T", part.Annotations[0])
	}
	if ann.URL != "https://example.com/paris" {
		t.Errorf("url: %q", ann.URL)
	}
	if ann.StartIndex != 21 || ann.EndIndex != 24 {
		t.Errorf("indices: start=%d end=%d", ann.StartIndex, ann.EndIndex)
	}
}

// CC-3: audio + prediction token details are mapped in ccUsageToCanonical.
func TestCCUsageToCanonical_AllFields(t *testing.T) {
	u := &Usage{
		PromptTokens:     120,
		CompletionTokens: 40,
		PromptDetails: &PromptTokenDetails{
			CachedTokens: 20,
			AudioTokens:  10,
		},
		CompletionDetails: &CompletionTokenDetails{
			ReasoningTokens:          5,
			AudioTokens:              3,
			AcceptedPredictionTokens: 7,
			RejectedPredictionTokens: 2,
		},
	}
	toks := ccUsageToCanonical(u)
	checks := map[string]int64{
		"input":               100, // 120 - 20 cached
		"output":              40,
		"cache_read":          20,
		"audio_input":         10,
		"reasoning":           5,
		"audio_output":        3,
		"accepted_prediction": 7,
		"rejected_prediction": 2,
	}
	for k, want := range checks {
		if toks[k] != want {
			t.Errorf("toks[%q] = %d, want %d", k, toks[k], want)
		}
	}
}

// CC-5: SerializeResponse returns an OpenAI error body when resp.Error is set.
func TestCCSerializeResponse_ErrorBody(t *testing.T) {
	resp := &v1.Response{
		ID:    "resp_err",
		Model: "gpt-4o",
		Error: &v1.Error{Code: "model_overloaded", Message: "too many requests"},
	}
	b, err := (CCTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeMap(t, b)
	errObj, ok := wire["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level 'error' key, got: %v", wire)
	}
	if errObj["message"] != "too many requests" {
		t.Errorf("message: %v", errObj["message"])
	}
	if errObj["code"] != "model_overloaded" {
		t.Errorf("code: %v", errObj["code"])
	}
	if _, has := wire["choices"]; has {
		t.Error("error body must not contain 'choices'")
	}
}

func TestCCParseResponse_OllamaReasoning(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":     "chatcmpl-r1",
		"object": "chat.completion",
		"model":  "gpt-oss",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":      "assistant",
				"content":   "The answer is 42.",
				"reasoning": "Let me think step by step...",
			},
			"finish_reason": "stop",
		}},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len: %d (want reasoning + message)", len(resp.Output))
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] is %T, want *v1.Reasoning", resp.Output[0])
	}
	if r.Content != "Let me think step by step..." {
		t.Errorf("reasoning content: %q", r.Content)
	}
	if got := ccReasoningField(r.ProviderData); got != ccReasoningFieldOllama {
		t.Errorf("preserved field: %q, want %q", got, ccReasoningFieldOllama)
	}
	if _, ok := resp.Output[1].(*v1.Message); !ok {
		t.Fatalf("output[1] is %T, want *v1.Message", resp.Output[1])
	}
}

func TestCCParseResponse_ReasoningContentField(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":     "chatcmpl-r2",
		"object": "chat.completion",
		"model":  "deepseek-r1",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":              "assistant",
				"content":           "Done.",
				"reasoning_content": "deliberating",
			},
			"finish_reason": "stop",
		}},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] is %T, want *v1.Reasoning", resp.Output[0])
	}
	if r.Content != "deliberating" {
		t.Errorf("reasoning content: %q", r.Content)
	}
	if got := ccReasoningField(r.ProviderData); got != ccReasoningFieldStd {
		t.Errorf("preserved field: %q, want %q", got, ccReasoningFieldStd)
	}
}

func TestCCSerializeResponse_ReasoningRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		pd        string
		wantField string
	}{
		{"ollama", `{"cc_reasoning_field":"reasoning"}`, "reasoning"},
		{"default", "", "reasoning_content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &v1.Response{
				ID:           "resp-1",
				Model:        "gpt-oss",
				FinishReason: v1.FinishReasonStop,
				Output: []v1.Item{
					&v1.Reasoning{Content: "because", ProviderData: json.RawMessage(tc.pd)},
					&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi"}}},
				},
			}
			body, err := (CCTranslator{}).SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Choices []struct {
					Message struct {
						Reasoning        string `json:"reasoning"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(body, &probe); err != nil {
				t.Fatal(err)
			}
			m := probe.Choices[0].Message
			if tc.wantField == "reasoning" && m.Reasoning != "because" {
				t.Errorf("want reasoning=because, got reasoning=%q reasoning_content=%q", m.Reasoning, m.ReasoningContent)
			}
			if tc.wantField == "reasoning_content" && m.ReasoningContent != "because" {
				t.Errorf("want reasoning_content=because, got reasoning=%q reasoning_content=%q", m.Reasoning, m.ReasoningContent)
			}
		})
	}
}

// TestCCParseResponse_ContentArray covers the gpt-oss / harmony divergence
// where a sync response carries message.content as an ARRAY of content parts
// rather than a string. Before the tolerant ChatResponseMessage.UnmarshalJSON
// this failed the whole parse ("cannot unmarshal array into Go struct field
// ...content of type string"), which dropped the caller onto the raw-body
// fallback and leaked vendor-shaped usage. It must now parse to canonical.
func TestCCParseResponse_ContentArray(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":     "chatcmpl-arr",
		"object": "chat.completion",
		"model":  "gpt-oss:120b-cloud",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "Hello, "},
					map[string]any{"type": "text", "text": "world."},
				},
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":             10,
			"completion_tokens":         5,
			"total_tokens":              15,
			"prompt_tokens_details":     map[string]any{"cached_tokens": 0},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	})
	resp, err := (CCTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatalf("array content must parse, got: %v", err)
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T, want *v1.Message", resp.Output[0])
	}
	part, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T, want *v1.OutputTextPart", msg.Content[0])
	}
	if part.Text != "Hello, world." {
		t.Errorf("concatenated text: %q", part.Text)
	}
	// Canonical usage is the flat orthogonal-meter map — never the nested
	// vendor detail objects the wire carried.
	if resp.Usage["input"] != 10 || resp.Usage["output"] != 5 {
		t.Errorf("usage not flattened to canonical: %v", resp.Usage)
	}
}

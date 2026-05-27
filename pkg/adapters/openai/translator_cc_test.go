package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
	"github.com/wyolet/relay/pkg/usage"
)

// mustJSON encodes v to JSON, panicking on error. Used for test fixture construction.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }
func boolPtr(v bool) *bool        { return &v }

// decodeMap decodes JSON bytes to a map for assertion without field coupling.
func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// --- ParseRequest ---

func TestCCParseRequest_SimpleMessage(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) != 1 || req.Model[0] != "gpt-4o" {
		t.Errorf("model: %v", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: got %d want 1", len(req.Input))
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T, want *v1.Message", req.Input[0])
	}
	if msg.Role != v1.RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	tp, ok := msg.Content[0].(*v1.TextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if tp.Text != "hi" {
		t.Errorf("text: %q", tp.Text)
	}
}

func TestCCParseRequest_SystemUserAssistantTurns(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "be helpful"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "hi there"},
		},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Instructions != "be helpful" {
		t.Errorf("instructions: %q", req.Instructions)
	}
	// user + assistant → 2 input items
	if len(req.Input) != 2 {
		t.Fatalf("input len: got %d want 2", len(req.Input))
	}
	userMsg, ok := req.Input[0].(*v1.Message)
	if !ok || userMsg.Role != v1.RoleUser {
		t.Errorf("input[0] role: %T %v", req.Input[0], userMsg)
	}
	assistantMsg, ok := req.Input[1].(*v1.Message)
	if !ok || assistantMsg.Role != v1.RoleAssistant {
		t.Errorf("input[1] role: %T", req.Input[1])
	}
}

func TestCCParseRequest_Tools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "search"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "search",
				"description": "Search the web",
				"parameters":  json.RawMessage(schema),
			},
		}},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("expected tools config")
	}
	if len(opts.Tools.Definitions) != 1 {
		t.Fatalf("tools len: %d", len(opts.Tools.Definitions))
	}
	ft, ok := opts.Tools.Definitions[0].(*v1.FunctionTool)
	if !ok {
		t.Fatalf("tool is %T", opts.Tools.Definitions[0])
	}
	if ft.Name != "search" {
		t.Errorf("tool name: %q", ft.Name)
	}
	if ft.Description != "Search the web" {
		t.Errorf("tool description: %q", ft.Description)
	}
}

func TestCCParseRequest_ToolChoice(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": map[string]any{}},
		}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "f"}},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Tools == nil || opts.Tools.Choice == nil {
		t.Fatal("expected tool choice")
	}
}

func TestCCParseRequest_ParallelToolCalls(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": map[string]any{}},
		}},
		"parallel_tool_calls": false,
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("expected tools")
	}
	if opts.Tools.Parallel == nil || *opts.Tools.Parallel != false {
		t.Errorf("parallel_tool_calls: %v", opts.Tools.Parallel)
	}
}

func TestCCParseRequest_SamplingFields(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":             "gpt-4o",
		"messages":          []any{map[string]any{"role": "user", "content": "x"}},
		"temperature":       0.7,
		"top_p":             0.9,
		"max_tokens":        512,
		"frequency_penalty": 0.1,
		"presence_penalty":  0.2,
		"seed":              int64(42),
		"stop":              []string{"END"},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Sampling == nil {
		t.Fatal("expected sampling")
	}
	s := opts.Sampling
	if s.Temperature == nil || *s.Temperature != 0.7 {
		t.Errorf("temperature: %v", s.Temperature)
	}
	if s.TopP == nil || *s.TopP != 0.9 {
		t.Errorf("top_p: %v", s.TopP)
	}
	if s.MaxTokens == nil || *s.MaxTokens != 512 {
		t.Errorf("max_tokens: %v", s.MaxTokens)
	}
	if s.FrequencyPenalty == nil || *s.FrequencyPenalty != 0.1 {
		t.Errorf("frequency_penalty: %v", s.FrequencyPenalty)
	}
	if s.PresencePenalty == nil || *s.PresencePenalty != 0.2 {
		t.Errorf("presence_penalty: %v", s.PresencePenalty)
	}
	if s.Seed == nil || *s.Seed != 42 {
		t.Errorf("seed: %v", s.Seed)
	}
	if len(s.Stop) != 1 || s.Stop[0] != "END" {
		t.Errorf("stop: %v", s.Stop)
	}
}

func TestCCParseRequest_ResponseFormatJSONSchema(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "my_schema",
				"schema": map[string]any{"type": "object"},
			},
		},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Output == nil || opts.Output.Format == nil {
		t.Fatal("expected output format")
	}
	if opts.Output.Format.Type != "json_schema" {
		t.Errorf("format type: %q", opts.Output.Format.Type)
	}
	if opts.Output.Format.Name != "my_schema" {
		t.Errorf("format name: %q", opts.Output.Format.Name)
	}
}

func TestCCParseRequest_StreamTrue(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"stream":   true,
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputMode != v1.OutputModeStream {
		t.Errorf("output_mode: %q", req.OutputMode)
	}
}

func TestCCParseRequest_UserAndMetadata(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"user":     "user-123",
		"metadata": map[string]string{"session": "abc"},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.User != "user-123" {
		t.Errorf("user: %q", req.User)
	}
	if req.Metadata["session"] != "abc" {
		t.Errorf("metadata: %v", req.Metadata)
	}
}

func TestCCParseRequest_ImageContentPart(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-4o",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "describe"},
				map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "https://example.com/img.png"},
				},
			},
		}},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	msg := req.Input[0].(*v1.Message)
	if len(msg.Content) != 2 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	img, ok := msg.Content[1].(*v1.ImagePart)
	if !ok {
		t.Fatalf("content[1] is %T, want *v1.ImagePart", msg.Content[1])
	}
	if img.ImageURL != "https://example.com/img.png" {
		t.Errorf("image url: %q", img.ImageURL)
	}
}

func TestCCParseRequest_MissingModel(t *testing.T) {
	body := mustJSON(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	})
	_, err := (CCTranslator{}).ParseRequest(body)
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestCCParseRequest_ToolMessage(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "use tool"},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":       "tc_1",
					"type":     "function",
					"function": map[string]any{"name": "f", "arguments": `{"x":1}`},
				}},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": "tc_1",
				"content":      "result",
			},
		},
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant(FunctionCall), tool(FunctionCallOutput)
	if len(req.Input) < 2 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	var foundOutput bool
	for _, item := range req.Input {
		if fco, ok := item.(*v1.FunctionCallOutput); ok {
			if fco.CallID != "tc_1" {
				t.Errorf("call_id: %q", fco.CallID)
			}
			if fco.Output != "result" {
				t.Errorf("output: %q", fco.Output)
			}
			foundOutput = true
		}
	}
	if !foundOutput {
		t.Error("expected FunctionCallOutput item")
	}
}

// --- SerializeRequest ---

func TestCCSerializeRequest_SimpleMessage(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-4o"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["model"] != "gpt-4o" {
		t.Errorf("model: %v", m["model"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len: %d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role: %v", msg["role"])
	}
}

func TestCCSerializeRequest_CacheConfigIsNoOp(t *testing.T) {
	// OpenAI prefix-caches automatically and exposes no breakpoint API, so a
	// neutral CacheConfig must be ignored — never emit cache_control / cache_config.
	req := &v1.Request{
		Model:        v1.ModelRefs{"gpt-4o"},
		Instructions: "be concise",
		CacheConfig:  &v1.CacheConfig{Instructions: true, Tools: true},
		ModelConfig: map[string]*v1.ModelOpts{
			"gpt-4o": {Tools: &v1.ToolsConfig{Definitions: v1.Tools{
				&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)},
			}}},
		},
		Input: []v1.Item{
			&v1.Message{
				Role:        v1.RoleUser,
				Content:     []v1.Part{&v1.TextPart{Text: "hi"}},
				CacheConfig: &v1.ItemCacheConfig{Anchor: true},
			},
		},
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "cache_control") || strings.Contains(s, "cache_config") {
		t.Errorf("cache vocabulary leaked into OpenAI CC output: %s", s)
	}
}

func TestCCSerializeRequest_WithInstructions(t *testing.T) {
	req := &v1.Request{
		Model:        v1.ModelRefs{"gpt-4o"},
		Instructions: "be concise",
		Input:        []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	msgs := m["messages"].([]any)
	// instructions become system message at index 0
	if len(msgs) < 1 {
		t.Fatal("no messages")
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("first msg role: %v", sys["role"])
	}
}

func TestCCSerializeRequest_StreamFlag(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"gpt-4o"},
		Input:      []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
		OutputMode: v1.OutputModeStream,
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["stream"] != true {
		t.Errorf("stream: %v", m["stream"])
	}
}

func TestCCSerializeRequest_MissingModel(t *testing.T) {
	req := &v1.Request{
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
	}
	_, err := (CCTranslator{}).SerializeRequest(req)
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestCCSerializeRequest_Tools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-4o"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
		ModelConfig: map[string]*v1.ModelOpts{
			"gpt-4o": {
				Tools: &v1.ToolsConfig{
					Definitions: []v1.Tool{&v1.FunctionTool{Name: "search", Parameters: schema}},
				},
			},
		},
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: %v", m["tools"])
	}
}

// --- ParseRequest/SerializeRequest round-trip ---

func TestCCRoundTrip_Request(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "be helpful"},
			map[string]any{"role": "user", "content": "hello"},
		},
		"temperature": 0.5,
		"max_tokens":  100,
	})

	tr := CCTranslator{}
	req, err := tr.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b2)
	if m["model"] != "gpt-4o" {
		t.Errorf("model: %v", m["model"])
	}
	msgs := m["messages"].([]any)
	// system instruction + user message
	if len(msgs) < 1 {
		t.Fatalf("messages: %v", msgs)
	}
}

// --- ParseResponse ---

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

func TestCCSerializeRequest_JSONSchemaFormat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-4o"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "x"}}}},
		ModelConfig: map[string]*v1.ModelOpts{
			"gpt-4o": {
				Output: &v1.OutputConfig{
					Format: &v1.Format{Type: "json_schema", Name: "s", Schema: schema},
				},
			},
		},
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	rf, ok := m["response_format"].(map[string]any)
	if !ok {
		t.Fatal("expected response_format")
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type: %v", rf["type"])
	}
}

func TestCCSerializeRequest_FunctionCallOutputInInput(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"gpt-4o"},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "use tool"}}},
			&v1.FunctionCall{ID: "fc_1", CallID: "tc_1", Name: "f", Arguments: `{"x":1}`, Status: v1.StatusCompleted},
			&v1.FunctionCallOutput{CallID: "tc_1", Output: "result"},
		},
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	msgs := m["messages"].([]any)
	var foundTool bool
	for _, msg := range msgs {
		mm := msg.(map[string]any)
		if mm["role"] == "tool" {
			foundTool = true
			if mm["tool_call_id"] != "tc_1" {
				t.Errorf("tool_call_id: %v", mm["tool_call_id"])
			}
		}
	}
	if !foundTool {
		t.Error("expected tool message")
	}
}

func TestCCNewToCanonicalStream_RefusalDelta(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	chunk := ccSSEChunk(map[string]any{
		"id":      "chatcmpl-ref",
		"object":  "chat.completion.chunk",
		"created": int64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"refusal": "I cannot help.",
			},
		}},
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	events := extractCanonicalEvents(out)
	// Refusal content maps to text delta in canonical.
	var foundDelta bool
	for _, e := range events {
		if e == v1.EventItemDelta {
			foundDelta = true
		}
	}
	if !foundDelta {
		t.Errorf("expected item.delta for refusal, got %v", events)
	}
}

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

func ccSSEChunk(data any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("data: %s\n\n", b))
}

func ccDoneChunk() []byte {
	return []byte("data: [DONE]\n\n")
}

func TestCCNewToCanonicalStream_TextSequence(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-s1",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": "Hello"},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-s1",
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

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

	// Must contain generation.created, item.started, item.delta(s), item.completed, generation.completed
	wantContains := []string{
		v1.EventGenerationCreated,
		v1.EventItemStarted,
		v1.EventItemDelta,
		v1.EventItemCompleted,
		v1.EventGenerationCompleted,
	}
	for _, want := range wantContains {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, events)
		}
	}
}

func TestCCNewToCanonicalStream_ToolCallSequence(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	chunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-tc",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"index": 0,
						"id":    "call_abc",
						"type":  "function",
						"function": map[string]any{
							"name":      "search",
							"arguments": `{"q":`,
						},
					}},
				},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":      "chatcmpl-tc",
			"object":  "chat.completion.chunk",
			"created": int64(1700000000),
			"model":   "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0,
						"function": map[string]any{
							"arguments": `"golang"}`,
						},
					}},
				},
			}},
		}),
		ccDoneChunk(),
	}

	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		events = append(events, extractCanonicalEvents(out)...)
	}

	wantContains := []string{v1.EventItemStarted, v1.EventItemDelta, v1.EventItemCompleted}
	for _, want := range wantContains {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, events)
		}
	}
}

func TestCCNewToCanonicalStream_FinishReasonThreaded(t *testing.T) {
	tr := CCTranslator{}

	cases := []struct {
		name       string
		ccReason   string
		wantFinish v1.FinishReason
		wantStatus v1.Status
	}{
		{"tool_calls", "tool_calls", v1.FinishReasonToolCalls, v1.StatusCompleted},
		{"length", "length", v1.FinishReasonLength, v1.StatusIncomplete},
		{"content_filter", "content_filter", v1.FinishReasonContentFilter, v1.StatusCompleted},
		{"stop", "stop", v1.FinishReasonStop, v1.StatusCompleted},
		// No finish_reason chunk at all -> default stop/completed.
		{"absent_defaults_stop", "", v1.FinishReasonStop, v1.StatusCompleted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := tr.NewToCanonicalStream()

			chunks := [][]byte{
				ccSSEChunk(map[string]any{
					"id":      "chatcmpl-fr",
					"object":  "chat.completion.chunk",
					"created": int64(1700000000),
					"model":   "gpt-4o",
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"role": "assistant", "content": "hi"},
					}},
				}),
			}
			if tc.ccReason != "" {
				chunks = append(chunks, ccSSEChunk(map[string]any{
					"id":      "chatcmpl-fr",
					"object":  "chat.completion.chunk",
					"created": int64(1700000000),
					"model":   "gpt-4o",
					"choices": []any{map[string]any{
						"index":         0,
						"delta":         map[string]any{},
						"finish_reason": tc.ccReason,
					}},
				}))
			}
			chunks = append(chunks, ccDoneChunk())

			var out []byte
			for _, c := range chunks {
				b, err := fn(c)
				if err != nil {
					t.Fatalf("translate: %v", err)
				}
				out = append(out, b...)
			}

			var completed *v1.GenerationCompletedEvent
			for _, frame := range splitCanonicalFrames(out) {
				event, data, ok := v1.ParseSSEChunk(frame)
				if !ok || event != v1.EventGenerationCompleted {
					continue
				}
				var ev v1.GenerationCompletedEvent
				if err := json.Unmarshal(data, &ev); err != nil {
					t.Fatalf("unmarshal completed: %v", err)
				}
				completed = &ev
			}
			if completed == nil {
				t.Fatal("no generation.completed event emitted")
			}
			if completed.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", completed.FinishReason, tc.wantFinish)
			}
			if completed.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", completed.Status, tc.wantStatus)
			}
		})
	}
}

func TestCCNewToCanonicalStream_ReasoningContent(t *testing.T) {
	tr := CCTranslator{}
	fn := tr.NewToCanonicalStream()

	// Chunk with non-standard reasoning_content field from o-series upstreams.
	chunk := ccSSEChunk(map[string]any{
		"id":      "chatcmpl-r1",
		"object":  "chat.completion.chunk",
		"created": int64(1700000000),
		"model":   "o1",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"reasoning_content": "Let me think...",
			},
		}},
	})

	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	events := extractCanonicalEvents(out)
	var foundDelta bool
	for _, e := range events {
		if e == v1.EventItemDelta {
			foundDelta = true
		}
	}
	if !foundDelta {
		t.Errorf("expected item.delta for reasoning_content, got %v", events)
	}
}

// CC-1: NewFromCanonicalStream must return a non-nil function (was nil → panic).
func TestCCNewFromCanonicalStream_NotNil(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()
	if fn == nil {
		t.Fatal("NewFromCanonicalStream returned nil")
	}
}

// CC-1: canonical event sequence → valid chat.completion.chunk frames + [DONE].
func TestCCNewFromCanonicalStream_TextSequence(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_1", Model: "gpt-4o"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello"})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_0", Index: 0, Item: &v1.Message{ID: "msg_0", Role: v1.RoleAssistant, Status: v1.StatusCompleted}})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "resp_1", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop, Usage: usage.Tokens{"input": 5, "output": 3}})

	allStr := string(allOut)
	if !strings.Contains(allStr, "chat.completion.chunk") {
		t.Errorf("expected chat.completion.chunk, got: %s", allStr)
	}
	if !strings.Contains(allStr, "Hello") {
		t.Errorf("expected delta text 'Hello'")
	}
	if !strings.Contains(allStr, "[DONE]") {
		t.Errorf("expected [DONE] terminator")
	}
	if !strings.Contains(allStr, "prompt_tokens") {
		t.Errorf("expected usage in final chunk")
	}
}

// CC-1: tool call streaming emits id+name on first chunk, arguments on delta chunks.
func TestCCNewFromCanonicalStream_ToolCallSequence(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()

	var allOut []byte
	feed := func(event string, data any) {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		allOut = append(allOut, out...)
	}

	feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "resp_tc", Model: "gpt-4o"})
	feed(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "fc_0", ItemType: v1.ItemTypeFunctionCall, Index: 0, Name: "get_weather"})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_0", Index: 0, Kind: v1.DeltaKindArguments, Delta: `{"loc`})
	feed(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "fc_0", Index: 0, Kind: v1.DeltaKindArguments, Delta: `ation":"NYC"}`})
	feed(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "fc_0", Index: 0, Item: &v1.FunctionCall{ID: "fc_0", CallID: "call_abc", Name: "get_weather", Arguments: `{"location":"NYC"}`, Status: v1.StatusCompleted}})
	feed(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{ID: "resp_tc", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonToolCalls})

	allStr := string(allOut)
	if !strings.Contains(allStr, "get_weather") {
		t.Errorf("expected tool name 'get_weather'")
	}
	if !strings.Contains(allStr, "tool_calls") {
		t.Errorf("expected finish_reason tool_calls")
	}
	if !strings.Contains(allStr, "[DONE]") {
		t.Errorf("expected [DONE]")
	}
}

// CC-1: error event produces an error body + [DONE].
func TestCCNewFromCanonicalStream_ErrorEvent(t *testing.T) {
	fn := (CCTranslator{}).NewFromCanonicalStream()

	feed := func(event string, data any) []byte {
		out, err := fn(canonicalChunk(event, data))
		if err != nil {
			t.Fatalf("translate %s: %v", event, err)
		}
		return out
	}
	var allOut []byte
	allOut = append(allOut, feed(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "r", Model: "m"})...)
	allOut = append(allOut, feed(v1.EventError, v1.ErrorEvent{Code: "server_error", Message: "boom"})...)

	allStr := string(allOut)
	if !strings.Contains(allStr, "boom") {
		t.Errorf("error message missing: %s", allStr)
	}
	if !strings.Contains(allStr, "[DONE]") {
		t.Errorf("[DONE] missing after error")
	}
}

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

// CC-4: SerializeRequest sets stream_options.include_usage=true for stream mode.
func TestCCSerializeRequest_StreamIncludesUsage(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"gpt-4o"},
		Input:      []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
		OutputMode: v1.OutputModeStream,
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeMap(t, b)
	so, ok := wire["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or wrong type: %v", wire["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage: %v", so["include_usage"])
	}
}

// CC-4: non-streaming requests must NOT carry stream_options.
func TestCCSerializeRequest_NoStreamOptions_Sync(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"gpt-4o"},
		Input:      []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
		OutputMode: v1.OutputModeSync,
	}
	b, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeMap(t, b)
	if _, ok := wire["stream_options"]; ok {
		t.Errorf("stream_options must be absent for sync requests")
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

// extractCanonicalEvents parses concatenated canonical SSE bytes and collects event names.
func extractCanonicalEvents(b []byte) []string {
	var names []string
	for _, frame := range splitCanonicalFrames(b) {
		event, _, ok := v1.ParseSSEChunk(frame)
		if ok && event != "" {
			names = append(names, event)
		}
	}
	return names
}

// splitCanonicalFrames splits concatenated SSE bytes at \n\n.
func splitCanonicalFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		idx := indexDoubleNewline(b)
		if idx < 0 {
			if len(strings.TrimSpace(string(b))) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		frame := b[:idx+2]
		if len(strings.TrimSpace(string(b[:idx]))) > 0 {
			frames = append(frames, frame)
		}
		b = b[idx+2:]
	}
	return frames
}

func indexDoubleNewline(b []byte) int {
	for i := 0; i < len(b)-1; i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i
		}
	}
	return -1
}

// --- Ollama reasoning divergence (reasoning vs reasoning_content) ---

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

func TestCCToCanonicalStream_OllamaReasoning(t *testing.T) {
	fn := (CCTranslator{}).NewToCanonicalStream()
	chunks := [][]byte{
		ccSSEChunk(map[string]any{
			"id":     "chatcmpl-rs",
			"object": "chat.completion.chunk",
			"model":  "gpt-oss",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "reasoning": "thinking..."},
			}},
		}),
		ccSSEChunk(map[string]any{
			"id":     "chatcmpl-rs",
			"object": "chat.completion.chunk",
			"model":  "gpt-oss",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": "Answer"},
			}},
		}),
		ccDoneChunk(),
	}
	var out []byte
	for _, c := range chunks {
		b, err := fn(c)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		out = append(out, b...)
	}
	s := string(out)
	if !strings.Contains(s, `"reasoning"`) {
		t.Fatalf("expected a canonical reasoning delta in stream output:\n%s", s)
	}
	if !strings.Contains(s, `"cc_reasoning_field":"reasoning"`) {
		t.Fatalf("expected provider_data to preserve Ollama field:\n%s", s)
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

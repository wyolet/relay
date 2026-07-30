package openai

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

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
	if req.Tools == nil {
		t.Fatal("expected tools config")
	}
	if len(req.Tools.Definitions) != 1 {
		t.Fatalf("tools len: %d", len(req.Tools.Definitions))
	}
	ft, ok := req.Tools.Definitions[0].(*v1.FunctionTool)
	if !ok {
		t.Fatalf("tool is %T", req.Tools.Definitions[0])
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
	if req.Tools == nil || req.Tools.Choice == nil {
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
	if req.Tools == nil {
		t.Fatal("expected tools")
	}
	if req.Tools.Parallel == nil || *req.Tools.Parallel != false {
		t.Errorf("parallel_tool_calls: %v", req.Tools.Parallel)
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

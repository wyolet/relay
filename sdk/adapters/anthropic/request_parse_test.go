package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseRequest tests ----

func TestAnthropicParseRequest_SimpleMessage(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) != 1 || req.Model[0] != "claude-3-5-sonnet-20241022" {
		t.Errorf("model: %v", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T", req.Input[0])
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
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
}

func TestAnthropicParseRequest_SystemMessage(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"system":     "You are a helpful assistant.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Instructions != "You are a helpful assistant." {
		t.Errorf("instructions: %q", req.Instructions)
	}
}

func TestAnthropicParseRequest_SystemAsBlocks(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"system": []any{
			map[string]any{"type": "text", "text": "Part one."},
			map[string]any{"type": "text", "text": "Part two."},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Instructions, "Part one.") || !strings.Contains(req.Instructions, "Part two.") {
		t.Errorf("instructions: %q", req.Instructions)
	}
}

func TestAnthropicParseRequest_MultiTurn(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "hi there"},
			map[string]any{"role": "user", "content": "how are you?"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 3 {
		t.Fatalf("input len: %d want 3", len(req.Input))
	}
}

func TestAnthropicParseRequest_SamplingParams(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":          "claude-3-5-sonnet-20241022",
		"max_tokens":     512,
		"temperature":    0.7,
		"top_p":          0.9,
		"top_k":          40,
		"stop_sequences": []string{"END", "STOP"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	model := "claude-3-5-sonnet-20241022"
	opts := req.ModelConfig[model]
	if opts == nil || opts.Sampling == nil {
		t.Fatal("no sampling opts")
	}
	s := opts.Sampling
	if s.Temperature == nil || *s.Temperature != 0.7 {
		t.Errorf("temperature: %v", s.Temperature)
	}
	if s.TopP == nil || *s.TopP != 0.9 {
		t.Errorf("top_p: %v", s.TopP)
	}
	if s.TopK == nil || *s.TopK != 40 {
		t.Errorf("top_k: %v", s.TopK)
	}
	if s.MaxTokens == nil || *s.MaxTokens != 512 {
		t.Errorf("max_tokens: %v", s.MaxTokens)
	}
	if len(s.Stop) != 2 {
		t.Errorf("stop: %v", s.Stop)
	}
}

func TestAnthropicParseRequest_ToolDefinitions(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"tools": []any{
			map[string]any{
				"name":         "search",
				"description":  "Search the web",
				"input_schema": json.RawMessage(schema),
			},
		},
		"tool_choice": map[string]any{"type": "auto"},
		"messages": []any{
			map[string]any{"role": "user", "content": "search for something"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Tools == nil {
		t.Fatal("no tool opts")
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
	if req.Tools.Choice == nil || req.Tools.Choice.Mode != "auto" {
		t.Errorf("tool choice: %v", req.Tools.Choice)
	}
}

func TestAnthropicParseRequest_ToolResult(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages": []any{
			map[string]any{"role": "user", "content": "Search something."},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_123",
						"name":  "search",
						"input": map[string]any{"q": "something"},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_123",
						"content":     "Found results.",
					},
				},
			},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	// Should have: user message, function call, function call output
	var foundFC, foundFCO bool
	for _, item := range req.Input {
		switch item.ItemType() {
		case v1.ItemTypeFunctionCall:
			foundFC = true
			fc := item.(*v1.FunctionCall)
			if fc.CallID != "toolu_123" {
				t.Errorf("call_id: %q", fc.CallID)
			}
		case v1.ItemTypeFunctionCallOutput:
			foundFCO = true
			fco := item.(*v1.FunctionCallOutput)
			if fco.CallID != "toolu_123" {
				t.Errorf("fco call_id: %q", fco.CallID)
			}
		}
	}
	if !foundFC {
		t.Error("no FunctionCall item")
	}
	if !foundFCO {
		t.Error("no FunctionCallOutput item")
	}
}

func TestAnthropicParseRequest_ImageContent(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "What's in this image?"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/jpeg",
							"data":       "abc123",
						},
					},
				},
			},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T", req.Input[0])
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content len: %d", len(msg.Content))
	}
	imgPart, ok := msg.Content[1].(*v1.ImagePart)
	if !ok {
		t.Fatalf("content[1] is %T", msg.Content[1])
	}
	if !strings.Contains(imgPart.ImageURL, "abc123") {
		t.Errorf("image URL: %q", imgPart.ImageURL)
	}
}

func TestAnthropicParseRequest_ThinkingEnabled(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-7-sonnet-20250219",
		"max_tokens": 100,
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": 2000,
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "think hard"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["claude-3-7-sonnet-20250219"]
	if opts == nil || opts.Reasoning == nil {
		t.Fatal("no reasoning config")
	}
	if opts.Reasoning.BudgetTokens == nil || *opts.Reasoning.BudgetTokens != 2000 {
		t.Errorf("budget_tokens: %v", opts.Reasoning.BudgetTokens)
	}
}

func TestAnthropicParseRequest_MetadataUserID(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"metadata":   map[string]any{"user_id": "user-abc"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.User != "user-abc" {
		t.Errorf("user: %q", req.User)
	}
}

func TestAnthropicParseRequest_StreamMode(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"stream":     true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputMode != v1.OutputModeStream {
		t.Errorf("output_mode: %q", req.OutputMode)
	}
}

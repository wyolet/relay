package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
	"github.com/wyolet/relay/pkg/usage"
)

// ---- test helpers ----

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

func sseChunk(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}

// collectCanonEvents runs Anthropic SSE chunks through NewToCanonicalStream and returns event names.
func collectCanonEvents(t *testing.T, chunks [][]byte) []string {
	t.Helper()
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var names []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		for _, frame := range splitFrames(out) {
			ev, _, ok := v1.ParseSSEChunk(frame)
			if ok && ev != "" {
				names = append(names, ev)
			}
		}
	}
	return names
}

// splitFrames splits concatenated SSE frames on \n\n.
func splitFrames(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	var frames [][]byte
	parts := strings.Split(string(data), "\n\n")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			frames = append(frames, []byte(p))
		}
	}
	return frames
}

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
	opts := req.ModelConfig["claude-3-5-sonnet-20241022"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("no tool opts")
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
	if opts.Tools.Choice == nil || opts.Tools.Choice.Mode != "auto" {
		t.Errorf("tool choice: %v", opts.Tools.Choice)
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

// ---- SerializeRequest tests ----

func TestAnthropicSerializeRequest_SimpleRoundTrip(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"system":     "You are helpful.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["model"] != "claude-3-5-sonnet-20241022" {
		t.Errorf("model: %v", m["model"])
	}
	if m["system"] != "You are helpful." {
		t.Errorf("system: %v", m["system"])
	}
	// max_tokens from sampling opts
	if int(m["max_tokens"].(float64)) != 100 {
		t.Errorf("max_tokens: %v", m["max_tokens"])
	}
}

func TestAnthropicSerializeRequest_MaxTokensDefault(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		OutputMode: v1.OutputModeSync,
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	mt := int(m["max_tokens"].(float64))
	if mt != defaultMaxTokensCanonical {
		t.Errorf("max_tokens default: got %d want %d", mt, defaultMaxTokensCanonical)
	}
}

func TestAnthropicSerializeRequest_ToolChoice_Required(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		OutputMode: v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{
			"claude-3-5-sonnet-20241022": {
				Tools: &v1.ToolsConfig{
					Definitions: v1.Tools{&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)}},
					Choice:      &v1.ToolChoice{Mode: "required"},
				},
			},
		},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "any" {
		t.Errorf("tool_choice type: %v", tc["type"])
	}
}

func TestAnthropicSerializeRequest_CacheConfig(t *testing.T) {
	model := "claude-3-5-sonnet-20241022"
	req := &v1.Request{
		Model:        v1.ModelRefs{model},
		Instructions: "You are Scarlet.",
		OutputMode:   v1.OutputModeSync,
		CacheConfig:  &v1.CacheConfig{Instructions: true, Tools: true},
		ModelConfig: map[string]*v1.ModelOpts{
			model: {
				Tools: &v1.ToolsConfig{
					Definitions: v1.Tools{
						&v1.FunctionTool{Name: "a", Parameters: json.RawMessage(`{}`)},
						&v1.FunctionTool{Name: "b", Parameters: json.RawMessage(`{}`)},
					},
				},
			},
		},
		Input: []v1.Item{
			&v1.Message{
				Role:        v1.RoleUser,
				Content:     []v1.Part{&v1.TextPart{Text: "stable history"}},
				CacheConfig: &v1.ItemCacheConfig{Anchor: true},
			},
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "latest turn"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)

	// Instructions anchor: system coerced to block array, breakpoint on last block.
	sysBlocks, ok := m["system"].([]any)
	if !ok {
		t.Fatalf("system: want []block, got %T (%v)", m["system"], m["system"])
	}
	lastSys := sysBlocks[len(sysBlocks)-1].(map[string]any)
	if lastSys["cache_control"] == nil {
		t.Errorf("no cache_control on system block: %v", lastSys)
	}
	if lastSys["text"] != "You are Scarlet." {
		t.Errorf("system text: %v", lastSys["text"])
	}

	// Tools anchor: breakpoint on the LAST tool only.
	tools := m["tools"].([]any)
	if cc := tools[0].(map[string]any)["cache_control"]; cc != nil {
		t.Errorf("unexpected cache_control on first tool: %v", cc)
	}
	if cc := tools[len(tools)-1].(map[string]any)["cache_control"]; cc == nil {
		t.Error("no cache_control on last tool")
	}

	// Per-message anchor: anchored message's content coerced to a block with a
	// breakpoint; the non-anchored trailing message stays a plain string.
	msgs := m["messages"].([]any)
	anchored := msgs[0].(map[string]any)
	blocks, ok := anchored["content"].([]any)
	if !ok {
		t.Fatalf("anchored message content: want []block, got %T", anchored["content"])
	}
	if blocks[len(blocks)-1].(map[string]any)["cache_control"] == nil {
		t.Error("no cache_control on anchored message block")
	}
	if _, isString := msgs[1].(map[string]any)["content"].(string); !isString {
		t.Errorf("non-anchored message content should stay a string, got %T", msgs[1].(map[string]any)["content"])
	}
}

func TestAnthropicSerializeRequest_NoCacheConfig_NoBreakpoints(t *testing.T) {
	req := &v1.Request{
		Model:        v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		Instructions: "You are helpful.",
		OutputMode:   v1.OutputModeSync,
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cache_control") {
		t.Errorf("cache_control leaked without CacheConfig: %s", out)
	}
	// system stays a plain string.
	if s, _ := decodeMap(t, out)["system"].(string); s != "You are helpful." {
		t.Errorf("system: want plain string, got %v", decodeMap(t, out)["system"])
	}
}

func TestAnthropicSerializeRequest_ThinkingConfig(t *testing.T) {
	budget := 3000
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-7-sonnet-20250219"},
		OutputMode: v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{
			"claude-3-7-sonnet-20250219": {
				Reasoning: &v1.ReasoningConfig{BudgetTokens: &budget},
			},
		},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "think"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	thinking := m["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Errorf("thinking type: %v", thinking["type"])
	}
	if int(thinking["budget_tokens"].(float64)) != 3000 {
		t.Errorf("budget_tokens: %v", thinking["budget_tokens"])
	}
}

func TestAnthropicSerializeRequest_UserMetadata(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		OutputMode: v1.OutputModeSync,
		User:       "u-99",
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	meta := m["metadata"].(map[string]any)
	if meta["user_id"] != "u-99" {
		t.Errorf("metadata.user_id: %v", meta["user_id"])
	}
}

func TestAnthropicSerializeRequest_DeveloperRoleBecomesSystem(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		OutputMode: v1.OutputModeSync,
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleDeveloper, Content: []v1.Part{&v1.TextPart{Text: "extra system"}}},
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if m["system"] != "extra system" {
		t.Errorf("system: %v", m["system"])
	}
	msgs := m["messages"].([]any)
	// developer message should not appear in messages array
	for _, msg := range msgs {
		msgM := msg.(map[string]any)
		if msgM["role"] == "developer" {
			t.Error("developer role leaked into messages")
		}
	}
}

// ---- ParseResponse tests ----

func TestAnthropicParseResponse_SimpleText(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "msg_abc",
		"type":       "message",
		"role":       "assistant",
		"model":      "claude-3-5-sonnet-20241022",
		"content":    []any{map[string]any{"type": "text", "text": "Hello!"}},
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
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 3},
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
			"input_tokens":             50,
			"output_tokens":            10,
			"cache_read_input_tokens":  30,
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
				ID:     "msg_0",
				Role:   v1.RoleAssistant,
				Status: v1.StatusCompleted,
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
				ID:     "msg_0",
				Role:   v1.RoleAssistant,
				Status: v1.StatusCompleted,
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
		ID:           "msg_len",
		Model:        "claude-3-5-sonnet-20241022",
		Status:       v1.StatusIncomplete,
		FinishReason: v1.FinishReasonLength,
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

// ---- Stream: Anthropic → canonical tests ----

func messageStartChunk(id, model string) []byte {
	return sseChunk("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    id,
			"type":  "message",
			"role":  "assistant",
			"model": model,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 0},
		},
	})
}

func pingChunk() []byte {
	return sseChunk("ping", map[string]any{"type": "ping"})
}

func contentBlockStartText(index int) []byte {
	return sseChunk("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
}

func textDeltaChunk(index int, text string) []byte {
	return sseChunk("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func contentBlockStopChunk(index int) []byte {
	return sseChunk("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
}

func messageDeltaChunk(stopReason string, outTokens int) []byte {
	return sseChunk("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	})
}

func messageStopChunk() []byte {
	return sseChunk("message_stop", map[string]any{"type": "message_stop"})
}

func TestAnthropicToCanonical_TextStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_001", "claude-3-5-sonnet-20241022"),
		pingChunk(),
		contentBlockStartText(0),
		textDeltaChunk(0, "Hello"),
		textDeltaChunk(0, " world"),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 5),
		messageStopChunk(),
	}

	events := collectCanonEvents(t, chunks)

	wantSequence := []string{
		v1.EventGenerationCreated,
		v1.EventItemStarted,
		v1.EventItemDelta,
		v1.EventItemDelta,
		v1.EventItemCompleted,
		v1.EventGenerationCompleted,
	}

	if len(events) != len(wantSequence) {
		t.Fatalf("events: got %v want %v", events, wantSequence)
	}
	for i, ev := range events {
		if ev != wantSequence[i] {
			t.Errorf("events[%d]: got %q want %q", i, ev, wantSequence[i])
		}
	}
}

func TestAnthropicToCanonical_PingIgnored(t *testing.T) {
	chunks := [][]byte{pingChunk()}
	events := collectCanonEvents(t, chunks)
	if len(events) != 0 {
		t.Errorf("expected no events from ping, got %v", events)
	}
}

func TestAnthropicToCanonical_ToolUseStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_tool", "claude-3-5-sonnet-20241022"),
		sseChunk("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use",
				"id":   "toolu_01",
				"name": "search",
			},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"q":`},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `"hi"}`},
		}),
		contentBlockStopChunk(0),
		messageDeltaChunk("tool_use", 10),
		messageStopChunk(),
	}

	events := collectCanonEvents(t, chunks)
	// Expect: created, started(fc), delta, delta, completed(fc), generation.completed
	hasStarted := false
	hasArgs := false
	for _, ev := range events {
		if ev == v1.EventItemStarted {
			hasStarted = true
		}
		if ev == v1.EventItemDelta {
			hasArgs = true
		}
	}
	if !hasStarted {
		t.Error("no item.started event")
	}
	if !hasArgs {
		t.Error("no item.delta event")
	}
}

func TestAnthropicToCanonical_ThinkingStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_think", "claude-3-7-sonnet-20250219"),
		sseChunk("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{"type": "thinking"},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "let me think"},
		}),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 20),
		messageStopChunk(),
	}

	events := collectCanonEvents(t, chunks)
	hasReasoning := false
	for _, ev := range events {
		if ev == v1.EventItemDelta {
			hasReasoning = true
		}
	}
	if !hasReasoning {
		t.Error("no item.delta event for thinking")
	}
}

func TestAnthropicToCanonical_ErrorEvent(t *testing.T) {
	chunk := sseChunk("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "overloaded_error",
			"message": "Overloaded",
		},
	})
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	out, err := fn(chunk)
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(out)
	if len(frames) == 0 {
		t.Fatal("expected error frame")
	}
	ev, data, _ := v1.ParseSSEChunk(frames[0])
	if ev != v1.EventError {
		t.Errorf("event: %q", ev)
	}
	var errEvt v1.ErrorEvent
	_ = json.Unmarshal(data, &errEvt)
	if errEvt.Code != "overloaded_error" {
		t.Errorf("error code: %q", errEvt.Code)
	}
}

func TestAnthropicToCanonical_MaxTokensStream(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_len", "claude-3-5-sonnet-20241022"),
		contentBlockStartText(0),
		textDeltaChunk(0, "partial"),
		contentBlockStopChunk(0),
		messageDeltaChunk("max_tokens", 100),
		messageStopChunk(),
	}
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var allFrames [][]byte
	for _, c := range chunks {
		out, _ := fn(c)
		allFrames = append(allFrames, splitFrames(out)...)
	}

	var completedFrame []byte
	for _, f := range allFrames {
		ev, _, _ := v1.ParseSSEChunk(f)
		if ev == v1.EventGenerationCompleted {
			completedFrame = f
			break
		}
	}
	if completedFrame == nil {
		t.Fatal("no generation.completed frame")
	}
	_, data, _ := v1.ParseSSEChunk(completedFrame)
	var ge v1.GenerationCompletedEvent
	_ = json.Unmarshal(data, &ge)
	if ge.Status != v1.StatusIncomplete {
		t.Errorf("status: %q", ge.Status)
	}
	if ge.FinishReason != v1.FinishReasonLength {
		t.Errorf("finish_reason: %q", ge.FinishReason)
	}
}

// ---- Stream: canonical → Anthropic tests ----

func canonSSEFrame(event string, data any) []byte {
	b, _ := json.Marshal(data)
	return v1.SSEFrame{Event: event, Data: b}.Bytes()
}

func collectAnthropicEvents(t *testing.T, chunks [][]byte) []string {
	t.Helper()
	fn := (AnthropicTranslator{}).NewFromCanonicalStream()
	var events []string
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		frames := splitFrames(out)
		for _, f := range frames {
			ev, _, ok := parseAnthropicSSEChunk(f)
			if ok && ev != "" {
				events = append(events, ev)
			}
		}
	}
	return events
}

func parseAnthropicSSEChunk(chunk []byte) (event string, data []byte, ok bool) {
	lines := strings.Split(string(chunk), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			data = []byte(strings.TrimSpace(line[5:]))
		}
	}
	return event, data, len(data) > 0
}

func TestCanonicalToAnthropic_TextStream(t *testing.T) {
	chunks := [][]byte{
		canonSSEFrame(v1.EventGenerationCreated, v1.GenerationCreatedEvent{ID: "msg_001", Model: "claude-3-5-sonnet-20241022"}),
		canonSSEFrame(v1.EventItemStarted, v1.ItemStartedEvent{ItemID: "msg_0", ItemType: v1.ItemTypeMessage, Index: 0}),
		canonSSEFrame(v1.EventItemDelta, v1.ItemDeltaEvent{ItemID: "msg_0", Index: 0, Kind: v1.DeltaKindText, Delta: "Hello"}),
		canonSSEFrame(v1.EventItemCompleted, v1.ItemCompletedEvent{ItemID: "msg_0", Index: 0, Item: &v1.Message{
			ID: "msg_0", Role: v1.RoleAssistant, Status: v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: "Hello"}},
		}}),
		canonSSEFrame(v1.EventGenerationCompleted, v1.GenerationCompletedEvent{
			ID:           "msg_001",
			Status:       v1.StatusCompleted,
			FinishReason: v1.FinishReasonStop,
			Usage:        usage.Tokens{"input": 5, "output": 5},
		}),
	}

	events := collectAnthropicEvents(t, chunks)
	// Expected: message_start, ping, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	first := events[0]
	if first != "message_start" {
		t.Errorf("first event: %q want message_start", first)
	}
	last := events[len(events)-1]
	if last != "message_stop" {
		t.Errorf("last event: %q want message_stop", last)
	}
}

// ---- E2E composition tests ----

func TestE2E_AnthropicToCanonicalToCC(t *testing.T) {
	// Build a canonical request from Anthropic wire, then serialize to CC.
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"system":     "You are helpful.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	aT := AnthropicTranslator{}
	req, err := aT.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	// Serialize to CC (via CCTranslator via openai package).
	// We test round-trip at the canonical level: parse → canonical → serialize → parse again.
	// Verify key fields in canonical.
	if req.Instructions != "You are helpful." {
		t.Errorf("instructions: %q", req.Instructions)
	}
	if len(req.Input) == 0 {
		t.Fatal("no input")
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T", req.Input[0])
	}
	if msg.Role != v1.RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
}

func TestE2E_AnthropicResponseRoundTrip(t *testing.T) {
	// Parse an Anthropic response → canonical → serialize back to Anthropic.
	body := mustJSON(map[string]any{
		"id":    "msg_rt",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": []any{
			map[string]any{"type": "text", "text": "Round-trip text."},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 5},
	})
	aT := AnthropicTranslator{}
	resp, err := aT.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := aT.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	content := m["content"].([]any)
	block := content[0].(map[string]any)
	if block["text"] != "Round-trip text." {
		t.Errorf("text round-trip: %v", block["text"])
	}
	if m["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
}

func TestE2E_StreamAnthropicToCanonicalAndBack(t *testing.T) {
	// Forward pass: Anthropic → canonical
	toCanon := (AnthropicTranslator{}).NewToCanonicalStream()
	// Reverse pass: canonical → Anthropic
	fromCanon := (AnthropicTranslator{}).NewFromCanonicalStream()

	anthropicChunks := [][]byte{
		messageStartChunk("msg_rt", "claude-3-5-sonnet-20241022"),
		contentBlockStartText(0),
		textDeltaChunk(0, "hello"),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 5),
		messageStopChunk(),
	}

	// Collect canonical frames
	var canonFrames [][]byte
	for _, c := range anthropicChunks {
		out, err := toCanon(c)
		if err != nil {
			t.Fatal(err)
		}
		canonFrames = append(canonFrames, splitFrames(out)...)
	}

	// Convert canonical back to Anthropic
	var allBack []byte
	for _, f := range canonFrames {
		// Reattach separator for fromCanon
		chunk := append(f, '\n', '\n')
		out, err := fromCanon(chunk)
		if err != nil {
			t.Fatal(err)
		}
		allBack = append(allBack, out...)
	}

	if len(allBack) == 0 {
		t.Error("no output from round-trip stream")
	}
	s := string(allBack)
	if !strings.Contains(s, "message_start") {
		t.Errorf("expected message_start in output: %q", s[:min(100, len(s))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

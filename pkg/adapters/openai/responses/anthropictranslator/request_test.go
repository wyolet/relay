package anthropictranslator

import (
	"encoding/json"
	"testing"

	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
)

// helper to build a pointer to bool
func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

// decodeRequest decodes the raw output bytes into a map for inspection.
func decodeRequest(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return m
}

// ---- test cases ----

func TestRequestToAnthropic_SimpleText(t *testing.T) {
	req := &pkgopenai.ResponsesRequest{
		Model:        "claude-opus-4-5",
		Instructions: "You are a helpful assistant.",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleUser,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Hello!"}},
			},
		},
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)

	if m["model"] != "claude-opus-4-5" {
		t.Errorf("model: got %q", m["model"])
	}
	if m["system"] != "You are a helpful assistant." {
		t.Errorf("system: got %q", m["system"])
	}
	// max_tokens defaults to 4096 when not specified
	if int(m["max_tokens"].(float64)) != defaultMaxTokens {
		t.Errorf("max_tokens: got %v want %d", m["max_tokens"], defaultMaxTokens)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len: got %d want 1", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role: got %q", msg["role"])
	}
	if msg["content"] != "Hello!" {
		t.Errorf("content: got %q", msg["content"])
	}
}

func TestRequestToAnthropic_FunctionTools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-3-5-sonnet-20241022",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleUser,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Search for something."}},
			},
		},
		Tools: pkgopenai.ResponsesTools{
			&pkgopenai.ResponsesFunctionTool{
				Name:        "search",
				Description: "Search the web",
				Parameters:  schema,
			},
		},
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)

	tools := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len: got %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "search" {
		t.Errorf("tool name: got %q", tool["name"])
	}
	if tool["description"] != "Search the web" {
		t.Errorf("tool description: got %q", tool["description"])
	}
	// Anthropic uses "input_schema" not "parameters"
	if _, ok := tool["input_schema"]; !ok {
		t.Error("missing input_schema field")
	}
	if _, ok := tool["parameters"]; ok {
		t.Error("unexpected parameters field (should be input_schema)")
	}
}

func TestRequestToAnthropic_MultimodalImage(t *testing.T) {
	t.Run("data_url", func(t *testing.T) {
		req := &pkgopenai.ResponsesRequest{
			Model: "claude-3-5-sonnet-20241022",
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{
					Role: pkgopenai.ResponsesRoleUser,
					Content: []pkgopenai.ResponsesPart{
						&pkgopenai.ResponsesTextPart{Text: "What is in this image?"},
						&pkgopenai.ResponsesImagePart{ImageURL: "data:image/jpeg;base64,/9j/abc"},
					},
				},
			},
		}
		b, err := RequestToAnthropic(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		m := decodeRequest(t, b)
		msgs := m["messages"].([]any)
		msg := msgs[0].(map[string]any)
		content := msg["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("content len: got %d want 2", len(content))
		}
		textBlock := content[0].(map[string]any)
		if textBlock["type"] != "text" {
			t.Errorf("block[0] type: got %q", textBlock["type"])
		}
		imgBlock := content[1].(map[string]any)
		if imgBlock["type"] != "image" {
			t.Errorf("block[1] type: got %q", imgBlock["type"])
		}
		src := imgBlock["source"].(map[string]any)
		if src["type"] != "base64" {
			t.Errorf("source type: got %q", src["type"])
		}
		if src["media_type"] != "image/jpeg" {
			t.Errorf("media_type: got %q", src["media_type"])
		}
	})

	t.Run("plain_url", func(t *testing.T) {
		req := &pkgopenai.ResponsesRequest{
			Model: "claude-3-5-sonnet-20241022",
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{
					Role: pkgopenai.ResponsesRoleUser,
					Content: []pkgopenai.ResponsesPart{
						&pkgopenai.ResponsesImagePart{ImageURL: "https://example.com/img.png"},
					},
				},
			},
		}
		b, err := RequestToAnthropic(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		m := decodeRequest(t, b)
		msgs := m["messages"].([]any)
		msg := msgs[0].(map[string]any)
		content := msg["content"].([]any)
		imgBlock := content[0].(map[string]any)
		src := imgBlock["source"].(map[string]any)
		if src["type"] != "url" {
			t.Errorf("source type: got %q", src["type"])
		}
		if src["url"] != "https://example.com/img.png" {
			t.Errorf("url: got %q", src["url"])
		}
	})
}

func TestRequestToAnthropic_ToolCallHistory(t *testing.T) {
	// A FunctionCall item followed by a FunctionCallOutput item should produce:
	// 1. assistant message with tool_use content block
	// 2. user message with tool_result content block
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-opus-4-5",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleUser,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Use the search tool."}},
			},
			&pkgopenai.ResponsesFunctionCall{
				ID:        "fc_01",
				CallID:    "call_abc",
				Name:      "search",
				Arguments: `{"q":"golang"}`,
			},
			&pkgopenai.ResponsesFunctionCallOutput{
				CallID: "call_abc",
				Output: "Go is a statically typed language.",
			},
		},
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)
	msgs := m["messages"].([]any)

	// Expected: [user, assistant(tool_use), user(tool_result)]
	if len(msgs) != 3 {
		t.Fatalf("messages len: got %d want 3", len(msgs))
	}

	assistantMsg := msgs[1].(map[string]any)
	if assistantMsg["role"] != "assistant" {
		t.Errorf("msg[1] role: got %q", assistantMsg["role"])
	}
	aContent := assistantMsg["content"].([]any)
	toolUse := aContent[0].(map[string]any)
	if toolUse["type"] != "tool_use" {
		t.Errorf("tool_use block type: got %q", toolUse["type"])
	}
	if toolUse["name"] != "search" {
		t.Errorf("tool name: got %q", toolUse["name"])
	}

	userMsg := msgs[2].(map[string]any)
	if userMsg["role"] != "user" {
		t.Errorf("msg[2] role: got %q", userMsg["role"])
	}
	uContent := userMsg["content"].([]any)
	toolResult := uContent[0].(map[string]any)
	if toolResult["type"] != "tool_result" {
		t.Errorf("tool_result block type: got %q", toolResult["type"])
	}
	if toolResult["tool_use_id"] != "call_abc" {
		t.Errorf("tool_use_id: got %q", toolResult["tool_use_id"])
	}
}

func TestRequestToAnthropic_ReasoningEffort(t *testing.T) {
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-opus-4-5",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleUser,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Think hard."}},
			},
		},
		Reasoning: &pkgopenai.ResponsesReasoningConfig{Effort: "high"},
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)

	thinking, ok := m["thinking"].(map[string]any)
	if !ok {
		t.Fatal("missing thinking field")
	}
	if thinking["effort"] != "high" {
		t.Errorf("thinking.effort: got %q", thinking["effort"])
	}
}

func TestRequestToAnthropic_ParallelToolCallsFalse(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-3-5-sonnet-20241022",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleUser,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Use tools."}},
			},
		},
		Tools: pkgopenai.ResponsesTools{
			&pkgopenai.ResponsesFunctionTool{Name: "mytool", Parameters: schema},
		},
		ParallelToolCalls: boolPtr(false),
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)

	tc, ok := m["tool_choice"].(map[string]any)
	if !ok {
		t.Fatal("missing tool_choice")
	}
	if tc["disable_parallel_tool_use"] != true {
		t.Errorf("disable_parallel_tool_use: got %v", tc["disable_parallel_tool_use"])
	}
}

func TestRequestToAnthropic_MaxOutputTokens(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		req := &pkgopenai.ResponsesRequest{
			Model:           "claude-opus-4-5",
			MaxOutputTokens: intPtr(2048),
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
			},
		}
		b, err := RequestToAnthropic(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		m := decodeRequest(t, b)
		if int(m["max_tokens"].(float64)) != 2048 {
			t.Errorf("max_tokens: got %v", m["max_tokens"])
		}
	})

	t.Run("default", func(t *testing.T) {
		req := &pkgopenai.ResponsesRequest{
			Model: "claude-opus-4-5",
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
			},
		}
		b, err := RequestToAnthropic(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		m := decodeRequest(t, b)
		if int(m["max_tokens"].(float64)) != defaultMaxTokens {
			t.Errorf("max_tokens default: got %v", m["max_tokens"])
		}
	})
}

func TestRequestToAnthropic_ToolChoiceMappings(t *testing.T) {
	cases := []struct {
		mode     string
		fn       string
		wantType string
	}{
		{"auto", "", "auto"},
		{"required", "", "any"},
		{"none", "", "none"},
		{"function", "mytool", "tool"},
	}

	schema := json.RawMessage(`{}`)
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			req := &pkgopenai.ResponsesRequest{
				Model: "claude-3-5-sonnet-20241022",
				Input: []pkgopenai.ResponsesItem{
					&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
				},
				Tools: pkgopenai.ResponsesTools{&pkgopenai.ResponsesFunctionTool{Name: "mytool", Parameters: schema}},
				ToolChoice: &pkgopenai.ResponsesToolChoice{Mode: tc.mode, FunctionName: tc.fn},
			}
			b, err := RequestToAnthropic(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			m := decodeRequest(t, b)
			toolChoice, ok := m["tool_choice"].(map[string]any)
			if !ok {
				t.Fatalf("tool_choice is not an object: %T %v", m["tool_choice"], m["tool_choice"])
			}
			if toolChoice["type"] != tc.wantType {
				t.Errorf("tool_choice.type: got %q want %q", toolChoice["type"], tc.wantType)
			}
			if tc.mode == "function" && toolChoice["name"] != "mytool" {
				t.Errorf("tool_choice.name: got %q", toolChoice["name"])
			}
		})
	}
}

func TestRequestToAnthropic_DeveloperRoleCoercedToSystem(t *testing.T) {
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-opus-4-5",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleDeveloper,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Be concise."}},
			},
			&pkgopenai.ResponsesMessage{
				Role:    pkgopenai.ResponsesRoleUser,
				Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Hello."}},
			},
		},
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)

	// developer role text should become system
	if m["system"] != "Be concise." {
		t.Errorf("system: got %q", m["system"])
	}
	// messages should only contain the user message
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len: got %d want 1", len(msgs))
	}
}

func TestRequestToAnthropic_RejectionFields(t *testing.T) {
	base := func() *pkgopenai.ResponsesRequest {
		return &pkgopenai.ResponsesRequest{
			Model: "claude-opus-4-5",
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
			},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*pkgopenai.ResponsesRequest)
		wantErr string
	}{
		{
			"previous_response_id",
			func(r *pkgopenai.ResponsesRequest) { r.PreviousResponseID = "resp_123" },
			"previous_response_id",
		},
		{
			"store_true",
			func(r *pkgopenai.ResponsesRequest) { r.Store = boolPtr(true) },
			"store",
		},
		{
			"conversation",
			func(r *pkgopenai.ResponsesRequest) { r.Conversation = "conv_123" },
			"conversation",
		},
		{
			"background_true",
			func(r *pkgopenai.ResponsesRequest) { r.Background = boolPtr(true) },
			"background",
		},
		{
			"truncation",
			func(r *pkgopenai.ResponsesRequest) { r.Truncation = "auto" },
			"truncation",
		},
		{
			"service_tier",
			func(r *pkgopenai.ResponsesRequest) { r.ServiceTier = "premium" },
			"service_tier",
		},
		{
			"safety_identifier",
			func(r *pkgopenai.ResponsesRequest) { r.SafetyIdentifier = "safe_123" },
			"safety_identifier",
		},
		{
			"prompt_cache_key",
			func(r *pkgopenai.ResponsesRequest) { r.PromptCacheKey = "pck_123" },
			"prompt_cache_key",
		},
		{
			"include",
			func(r *pkgopenai.ResponsesRequest) { r.Include = []string{"reasoning"} },
			"include",
		},
		{
			"logprobs",
			func(r *pkgopenai.ResponsesRequest) { r.Logprobs = boolPtr(true) },
			"logprobs",
		},
		{
			"top_logprobs",
			func(r *pkgopenai.ResponsesRequest) { r.TopLogprobs = intPtr(5) },
			"top_logprobs",
		},
		{
			"json_object_format",
			func(r *pkgopenai.ResponsesRequest) {
				r.Text = &pkgopenai.ResponsesTextConfig{Format: &pkgopenai.ResponsesFormat{Type: "json_object"}}
			},
			"json_object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mutate(req)
			_, err := RequestToAnthropic(req)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !containsString(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestRequestToAnthropic_StoreFalseNotRejected(t *testing.T) {
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-opus-4-5",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
		},
		Store: boolPtr(false),
	}
	_, err := RequestToAnthropic(req)
	if err != nil {
		t.Errorf("store=false should not be rejected: %v", err)
	}
}

func TestRequestToAnthropic_JSONSchemaFormat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-opus-4-5",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "Answer in JSON."}}},
		},
		Text: &pkgopenai.ResponsesTextConfig{
			Format: &pkgopenai.ResponsesFormat{
				Type:   "json_schema",
				Name:   "answer_schema",
				Schema: schema,
			},
		},
	}

	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)

	oc, ok := m["output_config"].(map[string]any)
	if !ok {
		t.Fatal("missing output_config")
	}
	format := oc["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Errorf("format type: got %q", format["type"])
	}
}

func TestRequestToAnthropic_MetadataUserID(t *testing.T) {
	t.Run("from_user_field", func(t *testing.T) {
		req := &pkgopenai.ResponsesRequest{
			Model: "claude-opus-4-5",
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
			},
			User: "user-123",
		}
		b, err := RequestToAnthropic(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		m := decodeRequest(t, b)
		meta := m["metadata"].(map[string]any)
		if meta["user_id"] != "user-123" {
			t.Errorf("user_id: got %q", meta["user_id"])
		}
	})

	t.Run("metadata_user_id_wins", func(t *testing.T) {
		req := &pkgopenai.ResponsesRequest{
			Model: "claude-opus-4-5",
			Input: []pkgopenai.ResponsesItem{
				&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
			},
			User:     "user-field",
			Metadata: map[string]string{"user_id": "meta-user"},
		}
		b, err := RequestToAnthropic(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		m := decodeRequest(t, b)
		meta := m["metadata"].(map[string]any)
		if meta["user_id"] != "meta-user" {
			t.Errorf("user_id: got %q (want meta-user to win)", meta["user_id"])
		}
	})
}

func TestRequestToAnthropic_ReasoningItemDropped(t *testing.T) {
	req := &pkgopenai.ResponsesRequest{
		Model: "claude-opus-4-5",
		Input: []pkgopenai.ResponsesItem{
			&pkgopenai.ResponsesMessage{Role: pkgopenai.ResponsesRoleUser, Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesTextPart{Text: "hi"}}},
			&pkgopenai.ResponsesReasoning{
				ID:               "rs_01",
				EncryptedContent: "encrypted-blob",
			},
		},
	}
	b, err := RequestToAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeRequest(t, b)
	msgs := m["messages"].([]any)
	// Reasoning items should be dropped; only the user message remains.
	if len(msgs) != 1 {
		t.Errorf("messages len: got %d want 1 (reasoning should be dropped)", len(msgs))
	}
}

// containsString returns true if s contains substr.
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

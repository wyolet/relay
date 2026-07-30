package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

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
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)}},
			Choice:      &v1.ToolChoice{Mode: "required"},
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
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{
				&v1.FunctionTool{Name: "a", Parameters: json.RawMessage(`{}`)},
				&v1.FunctionTool{Name: "b", Parameters: json.RawMessage(`{}`)},
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

func TestAnthropicSerializeRequest_CacheTTL(t *testing.T) {
	mkReq := func(ttl string) *v1.Request {
		return &v1.Request{
			Model:        v1.ModelRefs{"claude-3-5-sonnet-20241022"},
			Instructions: "You are Scarlet.",
			OutputMode:   v1.OutputModeSync,
			CacheConfig:  &v1.CacheConfig{Instructions: true, Tools: true, TTL: ttl},
			Tools: &v1.ToolsConfig{
				Definitions: v1.Tools{&v1.FunctionTool{Name: "a", Parameters: json.RawMessage(`{}`)}},
			},
			Input: []v1.Item{
				&v1.Message{
					Role:        v1.RoleUser,
					Content:     []v1.Part{&v1.TextPart{Text: "stable history"}},
					CacheConfig: &v1.ItemCacheConfig{Anchor: true},
				},
			},
		}
	}
	ccOf := func(t *testing.T, m map[string]any, path string) map[string]any {
		t.Helper()
		var block map[string]any
		switch path {
		case "system":
			blocks := m["system"].([]any)
			block = blocks[len(blocks)-1].(map[string]any)
		case "tools":
			tools := m["tools"].([]any)
			block = tools[len(tools)-1].(map[string]any)
		case "anchor":
			msgs := m["messages"].([]any)
			blocks := msgs[0].(map[string]any)["content"].([]any)
			block = blocks[len(blocks)-1].(map[string]any)
		}
		cc, _ := block["cache_control"].(map[string]any)
		if cc == nil {
			t.Fatalf("no cache_control at %s: %v", path, block)
		}
		return cc
	}

	// TTL beyond 5m upgrades every breakpoint to the 1h tier — including 24h,
	// which clamps down to Anthropic's largest tier.
	for _, ttl := range []string{"1h", "24h"} {
		out, err := (AnthropicTranslator{}).SerializeRequest(mkReq(ttl))
		if err != nil {
			t.Fatal(err)
		}
		m := decodeMap(t, out)
		for _, path := range []string{"system", "tools", "anchor"} {
			if got := ccOf(t, m, path)["ttl"]; got != "1h" {
				t.Errorf("TTL %s: cache_control.ttl at %s = %v, want 1h", ttl, path, got)
			}
		}
	}

	// TTL at/under the 5m default (and unset) emits no ttl field.
	for _, ttl := range []string{"", "5m"} {
		out, err := (AnthropicTranslator{}).SerializeRequest(mkReq(ttl))
		if err != nil {
			t.Fatal(err)
		}
		m := decodeMap(t, out)
		for _, path := range []string{"system", "tools", "anchor"} {
			if got, present := ccOf(t, m, path)["ttl"]; present {
				t.Errorf("TTL %q: unexpected cache_control.ttl at %s: %v", ttl, path, got)
			}
		}
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

// Effort-only reasoning (no explicit budget, as the agent sends) maps to
// adaptive thinking — the only mode the 4.7+/Sonnet 5/Fable 5 family accepts
// (budget_tokens 400s there). Summary requested → display "summarized", and
// the custom temperature that thinking forbids must be dropped.
func TestAnthropicSerializeRequest_ThinkingAdaptive(t *testing.T) {
	temp := 0.7
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-opus-4-8"},
		OutputMode: v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{
			"claude-opus-4-8": {
				Reasoning: &v1.ReasoningConfig{Effort: "medium", Summary: "auto"},
				Sampling:  &v1.SamplingParams{Temperature: &temp},
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
	thinking, ok := m["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("no thinking block: %v", m["thinking"])
	}
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking type: %v, want adaptive", thinking["type"])
	}
	if _, present := thinking["budget_tokens"]; present {
		t.Errorf("budget_tokens must be absent on adaptive, got %v", thinking["budget_tokens"])
	}
	if thinking["display"] != "summarized" {
		t.Errorf("display: %v, want summarized (Summary was requested)", thinking["display"])
	}
	if _, present := m["temperature"]; present {
		t.Errorf("temperature must be dropped when thinking is enabled, got %v", m["temperature"])
	}
}

// Explicit BudgetTokens keeps the legacy manual mode — the escape hatch for
// pre-4.6 models that reject adaptive. Budget floor and max_tokens headroom
// still apply.
func TestAnthropicSerializeRequest_ThinkingExplicitBudget(t *testing.T) {
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
	thinking, ok := m["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("no thinking block: %v", m["thinking"])
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking type: %v, want enabled", thinking["type"])
	}
	got, ok := thinking["budget_tokens"].(float64)
	if !ok || int(got) != 3000 {
		t.Errorf("budget_tokens: %v, want 3000", thinking["budget_tokens"])
	}
	if maxTokens := m["max_tokens"].(float64); maxTokens <= got {
		t.Errorf("max_tokens %v must exceed budget_tokens %v", maxTokens, got)
	}
}

// Empty-text thinking blocks (the 4.7+/Sonnet 5/Fable 5 default under display
// "omitted") must survive ParseResponse as Reasoning items carrying the
// signature — same-model replay echoes them back verbatim.
func TestAnthropicParseResponse_EmptyThinkingKeepsSignature(t *testing.T) {
	body := []byte(`{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-4-8",
		"content": [
			{"type": "thinking", "thinking": "", "signature": "SIG123"},
			{"type": "tool_use", "id": "toolu_1", "name": "f", "input": {}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 20}
	}`)
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var r *v1.Reasoning
	for _, item := range resp.Output {
		if v, ok := item.(*v1.Reasoning); ok {
			r = v
		}
	}
	if r == nil {
		t.Fatalf("empty-text thinking block dropped; output: %#v", resp.Output)
	}
	if !strings.Contains(string(r.ProviderData), "SIG123") {
		t.Errorf("signature missing from ProviderData: %s", r.ProviderData)
	}
}

// Replay: a Reasoning item with a signed thinking payload must be re-emitted
// in the SAME assistant message as its tool_use sibling — Anthropic requires
// the pairing on multi-turn tool loops.
func TestAnthropicSerializeRequest_ReasoningReplayWithToolUse(t *testing.T) {
	pd, _ := json.Marshal(map[string]string{
		"type": "thinking", "thinking": "", "signature": "SIG123",
	})
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-opus-4-8"},
		OutputMode: v1.OutputModeSync,
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
			&v1.Reasoning{ID: "rs_0", Status: v1.StatusCompleted, ProviderData: pd},
			&v1.FunctionCall{ID: "fc_1", CallID: "toolu_1", Name: "f", Arguments: "{}"},
			&v1.FunctionCallOutput{CallID: "toolu_1", Output: "ok"},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	msgs := m["messages"].([]any)
	// messages: [user "go", assistant [thinking, tool_use], user [tool_result]]
	if len(msgs) != 3 {
		t.Fatalf("messages: got %d, want 3: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("messages[1] role: %v", asst["role"])
	}
	blocks := asst["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks: got %d, want 2 (thinking + tool_use): %v", len(blocks), blocks)
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["signature"] != "SIG123" {
		t.Errorf("first block must be the signed thinking: %v", first)
	}
	if second := blocks[1].(map[string]any); second["type"] != "tool_use" {
		t.Errorf("second block must be tool_use: %v", second)
	}
}

// Cross-vendor reasoning (no signed Anthropic payload) is not replayable —
// unsigned thinking blocks are rejected upstream, so it must be dropped
// without corrupting the message sequence.
func TestAnthropicSerializeRequest_UnsignedReasoningDropped(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-opus-4-8"},
		OutputMode: v1.OutputModeSync,
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}},
			&v1.Reasoning{ID: "rs_0", Content: "openai reasoning text", Status: v1.StatusCompleted},
			&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "answer"}}},
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "next"}}},
		},
	}
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	if strings.Contains(string(out), `"thinking"`) {
		t.Errorf("unsigned reasoning must not emit a thinking block: %s", out)
	}
	if msgs := m["messages"].([]any); len(msgs) != 3 {
		t.Errorf("messages: got %d, want 3: %v", len(msgs), msgs)
	}
}

// Inbound adaptive thinking round-trips: {type:"adaptive", display:"summarized"}
// → ReasoningConfig{Summary:"auto"} → back out as adaptive+summarized.
func TestAnthropicParseRequest_AdaptiveThinkingRoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-8", "max_tokens": 1024,
		"thinking": {"type": "adaptive", "display": "summarized"},
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	rc := req.ModelConfig["claude-opus-4-8"].Reasoning
	if rc == nil {
		t.Fatal("adaptive thinking dropped at ParseRequest")
	}
	if rc.BudgetTokens != nil {
		t.Errorf("BudgetTokens must be nil for adaptive, got %v", *rc.BudgetTokens)
	}
	if rc.Summary == "" {
		t.Error("display summarized must map to a Summary request")
	}

	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	thinking := m["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Errorf("round-trip thinking: %v", thinking)
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

// ---- A-2 regression: disable_parallel_tool_use (fix: pass tc.Parallel not nil) ----

func boolPtr(v bool) *bool { return &v }

// TestSerializeRequest_ParallelFalse_DisablesParallel verifies that
// Parallel==false produces disable_parallel_tool_use:true on the wire.
func TestSerializeRequest_ParallelFalse_DisablesParallel(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		OutputMode: v1.OutputModeSync,
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)}},
			Choice:      &v1.ToolChoice{Mode: "auto"},
			Parallel:    boolPtr(false),
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
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing or wrong type: %v", m["tool_choice"])
	}
	if tc["disable_parallel_tool_use"] != true {
		t.Errorf("disable_parallel_tool_use: got %v want true", tc["disable_parallel_tool_use"])
	}
}

// TestSerializeRequest_ParallelNil_NoDisable verifies that nil Parallel does NOT
// emit disable_parallel_tool_use.
func TestSerializeRequest_ParallelNil_NoDisable(t *testing.T) {
	req := &v1.Request{
		Model:      v1.ModelRefs{"claude-3-5-sonnet-20241022"},
		OutputMode: v1.OutputModeSync,
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)}},
			Choice:      &v1.ToolChoice{Mode: "auto"},
			Parallel:    nil,
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
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing or wrong type: %v", m["tool_choice"])
	}
	if _, has := tc["disable_parallel_tool_use"]; has {
		t.Errorf("disable_parallel_tool_use present with nil Parallel: %v", tc)
	}
}

// ---- Structured output (forced-tool trick) tests ----

func makeStructuredOutputReq(model, formatType string, schema []byte) *v1.Request {
	f := &v1.Format{Type: formatType}
	if schema != nil {
		f.Schema = json.RawMessage(schema)
	}
	return &v1.Request{
		Model:      v1.ModelRefs{model},
		OutputMode: v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{
			model: {
				Output: &v1.OutputConfig{Format: f},
			},
		},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "answer"}}},
		},
	}
}

func TestSerializeRequest_StructuredOutput_JSONSchema(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	req := makeStructuredOutputReq("claude-3-5-sonnet-20241022", "json_schema", schema)
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)

	tools, ok := m["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools absent or empty: %v", m["tools"])
	}
	var synth map[string]any
	for _, tt := range tools {
		tm := tt.(map[string]any)
		if tm["name"] == structuredOutputToolName {
			synth = tm
		}
	}
	if synth == nil {
		t.Fatalf("synthetic tool %q not found in tools: %v", structuredOutputToolName, tools)
	}
	schemaJSON, _ := json.Marshal(synth["input_schema"])
	var got, want map[string]any
	_ = json.Unmarshal(schemaJSON, &got)
	_ = json.Unmarshal(schema, &want)
	if got["type"] != want["type"] {
		t.Errorf("input_schema type: got %v want %v", got["type"], want["type"])
	}

	tc, ok := m["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing: %v", m["tool_choice"])
	}
	if tc["type"] != "tool" || tc["name"] != structuredOutputToolName {
		t.Errorf("tool_choice: %v", tc)
	}
}

func TestSerializeRequest_StructuredOutput_JSONObject(t *testing.T) {
	req := makeStructuredOutputReq("claude-3-5-sonnet-20241022", "json_object", nil)
	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)

	tools := m["tools"].([]any)
	var synth map[string]any
	for _, tt := range tools {
		tm := tt.(map[string]any)
		if tm["name"] == structuredOutputToolName {
			synth = tm
		}
	}
	if synth == nil {
		t.Fatalf("synthetic tool not found")
	}
	schemaJSON, _ := json.Marshal(synth["input_schema"])
	var got map[string]any
	_ = json.Unmarshal(schemaJSON, &got)
	if got["type"] != "object" {
		t.Errorf("json_object schema: want {type:object}, got %v", got)
	}
}

func TestSerializeRequest_StructuredOutput_CallerForcesToolWins(t *testing.T) {
	schema := []byte(`{"type":"object"}`)
	model := "claude-3-5-sonnet-20241022"
	req := &v1.Request{
		Model:      v1.ModelRefs{model},
		OutputMode: v1.OutputModeSync,
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{Name: "my_tool", Parameters: json.RawMessage(`{}`)}},
			Choice:      &v1.ToolChoice{Mode: "function", FunctionName: "my_tool"},
		},
		ModelConfig: map[string]*v1.ModelOpts{
			model: {
				Output: &v1.OutputConfig{Format: &v1.Format{Type: "json_schema", Schema: schema}},
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

	if tools, ok := m["tools"].([]any); ok {
		for _, tt := range tools {
			if tt.(map[string]any)["name"] == structuredOutputToolName {
				t.Errorf("synthetic tool injected despite caller forcing their own tool")
			}
		}
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["name"] == structuredOutputToolName {
		t.Errorf("tool_choice points to synthetic tool; should point to caller's tool")
	}
	if tc["name"] != "my_tool" {
		t.Errorf("tool_choice.name: got %v want my_tool", tc["name"])
	}
}

func TestParseResponse_StructuredOutputTool_BecomesTextMessage(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_so",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_so",
				"name":  structuredOutputToolName,
				"input": map[string]any{"answer": "Paris"},
			},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d want 1", len(resp.Output))
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T, want *v1.Message", resp.Output[0])
	}
	tp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("msg.Content[0] is %T, want *v1.OutputTextPart", msg.Content[0])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tp.Text), &parsed); err != nil {
		t.Fatalf("OutputTextPart.Text is not valid JSON: %v (text: %q)", err, tp.Text)
	}
	if parsed["answer"] != "Paris" {
		t.Errorf("parsed answer: %v", parsed["answer"])
	}
	if resp.FinishReason != v1.FinishReasonStop {
		t.Errorf("finish_reason: got %q want stop", resp.FinishReason)
	}
	if resp.Status != v1.StatusCompleted {
		t.Errorf("status: got %q want completed", resp.Status)
	}
}

func TestParseResponse_NormalToolUse_Unchanged(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":    "msg_real",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_real",
				"name":  "lookup",
				"input": map[string]any{"q": "foo"},
			},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 3},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("output len: %d", len(resp.Output))
	}
	fc, ok := resp.Output[0].(*v1.FunctionCall)
	if !ok {
		t.Fatalf("output[0] is %T, want *v1.FunctionCall", resp.Output[0])
	}
	if fc.Name != "lookup" {
		t.Errorf("fc.Name: %q", fc.Name)
	}
	if resp.FinishReason != v1.FinishReasonToolCalls {
		t.Errorf("finish_reason: %q", resp.FinishReason)
	}
}

func TestStream_StructuredOutputTool_TextDeltas(t *testing.T) {
	chunks := [][]byte{
		messageStartChunk("msg_so", "claude-3-5-sonnet-20241022"),
		sseChunk("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "tool_use",
				"id":   "toolu_so",
				"name": structuredOutputToolName,
			},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"ans`},
		}),
		sseChunk("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `wer":"ok"}`},
		}),
		contentBlockStopChunk(0),
		messageDeltaChunk("tool_use", 10),
		messageStopChunk(),
	}

	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var allFrames [][]byte
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		allFrames = append(allFrames, splitFrames(out)...)
	}

	for _, f := range allFrames {
		ev, data, ok := v1.ParseSSEChunk(f)
		if !ok {
			continue
		}
		switch ev {
		case v1.EventItemStarted:
			var e v1.ItemStartedEvent
			_ = json.Unmarshal(data, &e)
			if e.ItemType != v1.ItemTypeMessage {
				t.Errorf("item.started ItemType: got %q want %q", e.ItemType, v1.ItemTypeMessage)
			}
		case v1.EventItemDelta:
			var e v1.ItemDeltaEvent
			_ = json.Unmarshal(data, &e)
			if e.Kind != v1.DeltaKindText {
				t.Errorf("item.delta Kind: got %q want text", e.Kind)
			}
		case v1.EventItemCompleted:
			var raw struct {
				Item json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal(data, &raw)
			var fields struct {
				CallID  string `json:"call_id"`
				Content []any  `json:"content"`
			}
			_ = json.Unmarshal(raw.Item, &fields)
			if fields.CallID != "" {
				t.Errorf("item.completed has call_id — should be a Message, not FunctionCall")
			}
		case v1.EventGenerationCompleted:
			var ge v1.GenerationCompletedEvent
			_ = json.Unmarshal(data, &ge)
			if ge.FinishReason != v1.FinishReasonStop {
				t.Errorf("generation.completed finish_reason: got %q want stop", ge.FinishReason)
			}
		}
	}
}

func TestAnthropicSerializeRequest_EagerInputStreaming(t *testing.T) {
	req := &v1.Request{
		Model: v1.ModelRefs{"claude-sonnet-5"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			&v1.FunctionTool{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	}

	decode := func(body []byte) []map[string]any {
		var wire struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("unmarshal wire: %v", err)
		}
		return wire.Tools
	}

	req.OutputMode = v1.OutputModeStream
	body, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize stream: %v", err)
	}
	tools := decode(body)
	if len(tools) != 1 || tools[0]["eager_input_streaming"] != true {
		t.Errorf("streaming request: want eager_input_streaming=true on tool, got %v", tools[0])
	}

	req.OutputMode = v1.OutputModeSync
	body, err = (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize sync: %v", err)
	}
	tools = decode(body)
	if _, present := tools[0]["eager_input_streaming"]; present {
		t.Errorf("sync request: eager_input_streaming must be omitted, got %v", tools[0])
	}
}

// completedFunctionCallStatus runs chunks through the stream translator and
// returns the status of the first completed function_call item.
func completedFunctionCallStatus(t *testing.T, chunks [][]byte) string {
	t.Helper()
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	for _, c := range chunks {
		out, err := fn(c)
		if err != nil {
			t.Fatalf("stream translate: %v", err)
		}
		for _, f := range splitFrames(out) {
			ev, data, ok := v1.ParseSSEChunk(f)
			if !ok || ev != v1.EventItemCompleted {
				continue
			}
			var raw struct {
				Item struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"item"`
			}
			_ = json.Unmarshal(data, &raw)
			if raw.Item.Type == string(v1.ItemTypeFunctionCall) {
				return raw.Item.Status
			}
		}
	}
	t.Fatal("no completed function_call item in stream")
	return ""
}

func TestAnthropicToCanonical_ToolUseStream_ArgsStatus(t *testing.T) {
	toolChunks := func(fragments ...string) [][]byte {
		chunks := [][]byte{
			messageStartChunk("msg_eager", "claude-sonnet-5"),
			sseChunk("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   "toolu_eager",
					"name": "search",
				},
			}),
		}
		for _, frag := range fragments {
			chunks = append(chunks, sseChunk("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
			}))
		}
		return append(chunks,
			contentBlockStopChunk(0),
			messageDeltaChunk("max_tokens", 10),
			messageStopChunk(),
		)
	}

	// Eager streaming means a truncated turn closes the block with
	// unterminated JSON — the completed item must say incomplete.
	if got := completedFunctionCallStatus(t, toolChunks(`{"q":`, `"unter`)); got != string(v1.StatusIncomplete) {
		t.Errorf("truncated args: status got %q want incomplete", got)
	}
	if got := completedFunctionCallStatus(t, toolChunks(`{"q":`, `"ok"}`)); got != string(v1.StatusCompleted) {
		t.Errorf("valid args: status got %q want completed", got)
	}
	// No-arg tools may stream zero input_json_delta frames; empty stays completed.
	if got := completedFunctionCallStatus(t, toolChunks()); got != string(v1.StatusCompleted) {
		t.Errorf("empty args: status got %q want completed", got)
	}
}

// A media-carrying tool_result (image block — e.g. a file read returning a
// PNG) must survive anthropic→canonical→anthropic: parse keeps the parts on
// FunctionCallOutput.Content, serialize re-emits the image block. Previously
// parse text-flattened the content and the image silently vanished.
func TestAnthropicToolResultImage_RoundTrip(t *testing.T) {
	body := `{
		"model": "claude-sonnet-5",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": "read a.png"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_img", "name": "fs_read", "input": {"path": "a.png"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_img", "content": [
					{"type": "text", "text": "here it is"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
				]}
			]}
		]
	}`
	req, err := (AnthropicTranslator{}).ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var fco *v1.FunctionCallOutput
	for _, item := range req.Input {
		if v, ok := item.(*v1.FunctionCallOutput); ok {
			fco = v
		}
	}
	if fco == nil {
		t.Fatal("no FunctionCallOutput parsed")
	}
	hasImage := false
	for _, p := range fco.Content {
		if _, ok := p.(*v1.ImagePart); ok {
			hasImage = true
		}
	}
	if !hasImage {
		t.Fatalf("image part lost at parse; content=%v output=%q", fco.Content, fco.Output)
	}

	out, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !strings.Contains(string(out), `"type":"image"`) {
		t.Fatalf("image block lost at serialize: %s", out)
	}
	if !strings.Contains(string(out), "here it is") {
		t.Fatalf("tool result text lost at serialize: %s", out)
	}
}

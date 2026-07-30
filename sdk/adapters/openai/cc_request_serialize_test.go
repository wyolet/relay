package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestCCCacheKeyRoundTrip(t *testing.T) {
	// Inbound: prompt_cache_key / prompt_cache_retention map to canonical
	// CacheConfig.Key / CacheConfig.TTL ("in_memory" → "5m").
	body := mustJSON(map[string]any{
		"model":                  "gpt-4o",
		"messages":               []any{map[string]any{"role": "user", "content": "hi"}},
		"prompt_cache_key":       "conv-abc123",
		"prompt_cache_retention": "in_memory",
	})
	req, err := (CCTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.CacheConfig == nil || req.CacheConfig.Key != "conv-abc123" {
		t.Fatalf("CacheConfig = %+v, want Key=conv-abc123", req.CacheConfig)
	}
	if req.CacheConfig.TTL != "5m" {
		t.Errorf("CacheConfig.TTL = %q, want 5m", req.CacheConfig.TTL)
	}

	// Outbound: canonical Key/TTL are emitted as prompt_cache_key/_retention.
	out, err := (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["prompt_cache_key"] != "conv-abc123" {
		t.Errorf("wire prompt_cache_key = %v, want conv-abc123", wire["prompt_cache_key"])
	}
	if wire["prompt_cache_retention"] != "in_memory" {
		t.Errorf("wire prompt_cache_retention = %v, want in_memory", wire["prompt_cache_retention"])
	}

	// Long TTL upgrades to the 24h tier.
	req.CacheConfig.TTL = "1h"
	out, err = (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["prompt_cache_retention"] != "24h" {
		t.Errorf("wire prompt_cache_retention = %v, want 24h", wire["prompt_cache_retention"])
	}

	// No key → field absent on the wire.
	req.CacheConfig = nil
	out, err = (CCTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if _, present := wire["prompt_cache_key"]; present {
		t.Errorf("prompt_cache_key present without CacheConfig.Key: %v", wire["prompt_cache_key"])
	}
	if _, present := wire["prompt_cache_retention"]; present {
		t.Errorf("prompt_cache_retention present without CacheConfig.TTL: %v", wire["prompt_cache_retention"])
	}
}

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
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			&v1.FunctionTool{Name: "fn", Parameters: json.RawMessage(`{}`)},
		}},
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
		Tools: &v1.ToolsConfig{
			Definitions: []v1.Tool{&v1.FunctionTool{Name: "search", Parameters: schema}},
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

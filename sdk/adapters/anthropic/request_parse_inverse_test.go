package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Every cache breakpoint SerializeRequest writes parses back to the canonical intent that produced it.
func TestParseRequest_CacheControlInvertsSerialize(t *testing.T) {
	anchor := &v1.ItemCacheConfig{Anchor: true}
	req := &v1.Request{
		Model:        v1.ModelRefs{"claude-sonnet-4-5"},
		Instructions: "Be terse.",
		CacheConfig:  &v1.CacheConfig{Instructions: true, Tools: true, TTL: "1h"},
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			&v1.FunctionTool{Name: "a", Parameters: json.RawMessage(`{}`)},
			&v1.FunctionTool{Name: "b", Parameters: json.RawMessage(`{}`)},
		}},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, CacheConfig: anchor, Content: []v1.Part{&v1.TextPart{Text: "q1"}}},
			&v1.Message{Role: v1.RoleAssistant, CacheConfig: anchor, Content: []v1.Part{&v1.OutputTextPart{Text: "a1"}}},
			&v1.FunctionCall{ID: "call_1", CallID: "call_1", Name: "a", Arguments: `{}`},
			&v1.FunctionCallOutput{CallID: "call_1", Output: "done"},
			&v1.Message{Role: v1.RoleSystem, CacheConfig: anchor, Content: []v1.Part{&v1.TextPart{Text: "rule"}}},
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "q2"}}},
		},
	}
	wire, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	back, err := (AnthropicTranslator{}).ParseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.CacheConfig, &v1.CacheConfig{Instructions: true, Tools: true, TTL: "1h"}) {
		t.Errorf("cache_config = %+v\nwire: %s", back.CacheConfig, wire)
	}
	var anchored []string
	for _, it := range back.Input {
		if m, ok := it.(*v1.Message); ok && m.CacheConfig != nil && m.CacheConfig.Anchor {
			anchored = append(anchored, string(m.Role))
		}
	}
	if want := []string{"user", "assistant", "system"}; !reflect.DeepEqual(anchored, want) {
		t.Errorf("anchored items = %v, want %v\nwire: %s", anchored, want, wire)
	}
}

// The default 5-minute tier writes no ttl, so it parses back to an unset TTL.
func TestParseRequest_CacheControlDefaultTTL(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"messages":[{"role":"user","content":"hi"}]}`)
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.CacheConfig, &v1.CacheConfig{Instructions: true}) {
		t.Errorf("cache_config = %+v", req.CacheConfig)
	}
}

func TestParseRequest_NoCacheControlLeavesCacheConfigNil(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":"hi"}]}`)
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.CacheConfig != nil {
		t.Errorf("cache_config = %+v, want nil", req.CacheConfig)
	}
	if req.Input[0].(*v1.Message).CacheConfig != nil {
		t.Errorf("unexpected item anchor")
	}
}

// Document blocks parse to the FileParts SerializeRequest turns into them.
func TestParseRequest_DocumentBlocksInvertSerialize(t *testing.T) {
	parts := []v1.Part{
		&v1.TextPart{Text: "Compare."},
		&v1.FilePart{FileData: "JVBERi0xLjQK", MediaType: "application/pdf"},
		&v1.FilePart{FileURL: "https://example.com/q3.pdf"},
	}
	req := &v1.Request{
		Model: v1.ModelRefs{"m"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: parts}},
	}
	wire, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	back, err := (AnthropicTranslator{}).ParseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	got := back.Input[0].(*v1.Message).Content
	if !reflect.DeepEqual(got, parts) {
		t.Errorf("parts = %s, want %s", mustJSON(got), mustJSON(parts))
	}
}

func TestParseRequest_DisableParallelToolUse(t *testing.T) {
	cases := []struct {
		choice string
		want   *bool
	}{
		{`{"type":"auto","disable_parallel_tool_use":true}`, boolPtr(false)},
		{`{"type":"any","disable_parallel_tool_use":true}`, boolPtr(false)},
		{`{"type":"tool","name":"f","disable_parallel_tool_use":false}`, boolPtr(true)},
		{`{"type":"auto"}`, nil},
	}
	for _, c := range cases {
		body := []byte(`{"model":"m","max_tokens":10,"tools":[{"name":"f","input_schema":{}}],"tool_choice":` + c.choice + `,"messages":[{"role":"user","content":"hi"}]}`)
		req, err := (AnthropicTranslator{}).ParseRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(req.Tools.Parallel, c.want) {
			t.Errorf("%s: parallel = %v, want %v", c.choice, deref(req.Tools.Parallel), deref(c.want))
		}
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

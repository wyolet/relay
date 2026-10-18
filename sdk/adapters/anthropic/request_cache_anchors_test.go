package anthropic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func toolLoopInput(anchorCall, anchorOutput bool) []v1.Item {
	var callAnchor, outAnchor *v1.ItemCacheConfig
	if anchorCall {
		callAnchor = &v1.ItemCacheConfig{Anchor: true}
	}
	if anchorOutput {
		outAnchor = &v1.ItemCacheConfig{Anchor: true}
	}
	return []v1.Item{
		&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "fix the bug"}}},
		&v1.FunctionCall{ID: "call_1", CallID: "call_1", Name: "read", Arguments: `{"path":"a.go"}`, CacheConfig: callAnchor},
		&v1.FunctionCallOutput{CallID: "call_1", Output: "package a", CacheConfig: outAnchor},
	}
}

func serializeMessages(t *testing.T, req *v1.Request) ([]byte, []any) {
	t.Helper()
	wire, err := (AnthropicTranslator{}).SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return wire, decodeMap(t, wire)["messages"].([]any)
}

func blockAt(t *testing.T, msgs []any, msg, block int) map[string]any {
	t.Helper()
	blocks, ok := msgs[msg].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("message %d content is not a block array: %v", msg, msgs[msg])
	}
	return blocks[block].(map[string]any)
}

func TestSerializeRequest_ToolItemAnchors(t *testing.T) {
	wire, msgs := serializeMessages(t, &v1.Request{
		Model:       v1.ModelRefs{"claude-sonnet-5-5"},
		CacheConfig: &v1.CacheConfig{TTL: "1h"},
		Input:       toolLoopInput(true, true),
	})
	use := blockAt(t, msgs, 1, 0)
	if use["type"] != "tool_use" || !reflect.DeepEqual(use["cache_control"], map[string]any{"type": "ephemeral", "ttl": "1h"}) {
		t.Errorf("tool_use block = %v", use)
	}
	result := blockAt(t, msgs, 2, 0)
	if result["type"] != "tool_result" || result["cache_control"] == nil {
		t.Errorf("tool_result block = %v", result)
	}
	if n := strings.Count(string(wire), "cache_control"); n != 2 {
		t.Errorf("cache_control count = %d, want 2\nwire: %s", n, wire)
	}
}

func TestSerializeRequest_HistoryMarksLastInputBlock(t *testing.T) {
	cases := map[string]struct {
		input    []v1.Item
		msg      int
		wantType string
	}{
		"tool result last": {toolLoopInput(false, false), 2, "tool_result"},
		"user text last": {
			[]v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
			0, "text",
		},
		"tool call last": {toolLoopInput(false, false)[:2], 1, "tool_use"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wire, msgs := serializeMessages(t, &v1.Request{
				Model:       v1.ModelRefs{"claude-sonnet-5-5"},
				CacheConfig: &v1.CacheConfig{History: true},
				Input:       tc.input,
			})
			last := blockAt(t, msgs, tc.msg, 0)
			if last["type"] != tc.wantType || last["cache_control"] == nil {
				t.Errorf("last block = %v", last)
			}
			if n := strings.Count(string(wire), "cache_control"); n != 1 {
				t.Errorf("cache_control count = %d, want 1\nwire: %s", n, wire)
			}
		})
	}
}

// Thinking blocks cannot carry cache_control, so History falls back to the block before them.
func TestSerializeRequest_HistorySkipsThinking(t *testing.T) {
	msgs := []anthropicCanonMsg{{Role: "assistant", Content: []map[string]any{
		{"type": "text", "text": "t"},
		{"type": "thinking", "thinking": "x", "signature": "s"},
	}}}
	markHistoryBreakpoint(msgs, "")
	blocks := msgs[0].Content.([]map[string]any)
	if blocks[0]["cache_control"] == nil || blocks[1]["cache_control"] != nil {
		t.Errorf("blocks = %v", blocks)
	}
}

// Instructions + Tools + History + three item anchors ask for six breakpoints; the request keeps
// the three request-level ones and the latest item anchor, and drops the two oldest anchors.
func TestSerializeRequest_BreakpointBudget(t *testing.T) {
	anchor := &v1.ItemCacheConfig{Anchor: true}
	req := &v1.Request{
		Model:        v1.ModelRefs{"claude-sonnet-5-5"},
		Instructions: "Be terse.",
		CacheConfig:  &v1.CacheConfig{Instructions: true, Tools: true, History: true},
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			&v1.FunctionTool{Name: "read", Parameters: json.RawMessage(`{}`)},
		}},
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, CacheConfig: anchor, Content: []v1.Part{&v1.TextPart{Text: "q1"}}},
			&v1.FunctionCall{ID: "c1", CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
			&v1.FunctionCallOutput{CallID: "c1", Output: "r1", CacheConfig: anchor},
			&v1.FunctionCall{ID: "c2", CallID: "c2", Name: "read", Arguments: `{}`},
			&v1.FunctionCallOutput{CallID: "c2", Output: "r2"},
		},
	}
	wire, msgs := serializeMessages(t, req)
	if n := strings.Count(string(wire), "cache_control"); n != anthropicMaxBreakpoints {
		t.Fatalf("cache_control count = %d, want %d\nwire: %s", n, anthropicMaxBreakpoints, wire)
	}
	m := decodeMap(t, wire)
	if m["system"].([]any)[0].(map[string]any)["cache_control"] == nil {
		t.Error("instructions breakpoint dropped")
	}
	if m["tools"].([]any)[0].(map[string]any)["cache_control"] == nil {
		t.Error("tools breakpoint dropped")
	}
	marked := map[string]bool{}
	for i := range msgs {
		blocks, ok := msgs[i].(map[string]any)["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			b := b.(map[string]any)
			if b["cache_control"] != nil {
				key, _ := b["text"].(string)
				if id, ok := b["id"].(string); ok {
					key = "use:" + id
				}
				if id, ok := b["tool_use_id"].(string); ok {
					key = "result:" + id
				}
				marked[key] = true
			}
		}
	}
	if want := map[string]bool{"result:c1": true, "result:c2": true}; !reflect.DeepEqual(marked, want) {
		t.Errorf("marked message blocks = %v, want %v\nwire: %s", marked, want, wire)
	}
}

// Canonical → wire → canonical keeps tool-item anchors on the items that carried them.
func TestParseRequest_ToolAnchorsInvertSerialize(t *testing.T) {
	wire, err := (AnthropicTranslator{}).SerializeRequest(&v1.Request{
		Model:       v1.ModelRefs{"claude-sonnet-5-5"},
		CacheConfig: &v1.CacheConfig{TTL: "1h"},
		Input:       toolLoopInput(true, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	back, err := (AnthropicTranslator{}).ParseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	call, ok := back.Input[1].(*v1.FunctionCall)
	if !ok || call.CacheConfig == nil || !call.CacheConfig.Anchor {
		t.Errorf("function call = %#v", back.Input[1])
	}
	out, ok := back.Input[2].(*v1.FunctionCallOutput)
	if !ok || out.CacheConfig == nil || !out.CacheConfig.Anchor {
		t.Errorf("function call output = %#v", back.Input[2])
	}
	if back.Input[0].(*v1.Message).CacheConfig != nil {
		t.Error("unanchored message picked up an anchor")
	}
	if back.CacheConfig == nil || back.CacheConfig.TTL != "1h" {
		t.Errorf("cache_config = %+v, want ttl 1h from the tool breakpoints", back.CacheConfig)
	}
}

// Wire → canonical → wire re-emits cache_control on exactly the tool blocks that had it.
func TestSerializeRequest_ToolAnchorsInvertParse(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{},"cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok","cache_control":{"type":"ephemeral"}}]}]}`)
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, msgs := serializeMessages(t, req)
	if blockAt(t, msgs, 1, 0)["cache_control"] == nil || blockAt(t, msgs, 2, 0)["cache_control"] == nil {
		t.Errorf("tool breakpoints lost\nwire: %s", wire)
	}
	if n := strings.Count(string(wire), "cache_control"); n != 2 {
		t.Errorf("cache_control count = %d, want 2\nwire: %s", n, wire)
	}
}

// History has no wire form of its own: it parses back as an anchor on the last input item, which
// serializes to the same breakpoint.
func TestParseRequest_HistoryReadsBackAsLastItemAnchor(t *testing.T) {
	wire, err := (AnthropicTranslator{}).SerializeRequest(&v1.Request{
		Model:       v1.ModelRefs{"claude-sonnet-5-5"},
		CacheConfig: &v1.CacheConfig{History: true},
		Input:       toolLoopInput(false, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	back, err := (AnthropicTranslator{}).ParseRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := back.Input[2].(*v1.FunctionCallOutput)
	if !ok || out.CacheConfig == nil || !out.CacheConfig.Anchor {
		t.Errorf("last item = %#v, want an anchored function call output", back.Input[2])
	}
	again, err := (AnthropicTranslator{}).SerializeRequest(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(wire) {
		t.Errorf("re-serialized wire differs\nfirst:  %s\nsecond: %s", wire, again)
	}
}

package openai

import (
	"encoding/json"
	"errors"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func ccUser(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

func TestCCSerializeRequest(t *testing.T) {
	anchor := &v1.ItemCacheConfig{Anchor: true}
	cases := []struct {
		name string
		req  *v1.Request
		wire string
	}{
		{
			name: "hoisted system joins the instructions, the rest stays in place",
			req: &v1.Request{Model: v1.ModelRefs{"m"}, Instructions: "base", Input: []v1.Item{
				ccUser("a"),
				&v1.Message{Role: v1.RoleSystem, Hoist: true, Content: []v1.Part{&v1.TextPart{Text: "typed"}}},
				&v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: "positional"}}},
				&v1.Message{Role: v1.RoleDeveloper, Content: []v1.Part{&v1.TextPart{Text: "dev"}}},
			}},
			wire: `{"model":"m","messages":[{"role":"system","content":"base\ntyped"},{"role":"user","content":"a"},{"role":"system","content":"positional"},{"role":"system","content":"dev"}]}`,
		},
		{
			name: "a hoisted system item alone becomes the leading system message",
			req: &v1.Request{Model: v1.ModelRefs{"m"}, Input: []v1.Item{
				ccUser("a"),
				&v1.Message{Role: v1.RoleSystem, Hoist: true, Content: []v1.Part{&v1.TextPart{Text: "typed"}}},
			}},
			wire: `{"model":"m","messages":[{"role":"system","content":"typed"},{"role":"user","content":"a"}]}`,
		},
		{
			name: "cache breakpoints and anchors have no wire form; a long TTL takes the 24h tier",
			req: &v1.Request{
				Model:       v1.ModelRefs{"m"},
				CacheConfig: &v1.CacheConfig{Instructions: true, Tools: true, History: true, Key: "conv-1", TTL: "1h"},
				Input: []v1.Item{
					&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "go"}}, CacheConfig: anchor},
					&v1.FunctionCall{ID: "fc_1", CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
					&v1.FunctionCallOutput{CallID: "c1", Output: "ok", CacheConfig: anchor},
				},
			},
			wire: `{"model":"m","prompt_cache_key":"conv-1","prompt_cache_retention":"24h","messages":[
				{"role":"user","content":"go"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"ok"}]}`,
		},
		{
			name: "a TTL up to 10m takes the in_memory tier",
			req:  &v1.Request{Model: v1.ModelRefs{"m"}, CacheConfig: &v1.CacheConfig{TTL: "10m"}},
			wire: `{"model":"m","messages":null,"prompt_cache_retention":"in_memory"}`,
		},
		{
			name: "an unparseable TTL is left to the vendor default",
			req:  &v1.Request{Model: v1.ModelRefs{"m"}, CacheConfig: &v1.CacheConfig{TTL: "soon"}},
			wire: `{"model":"m","messages":null}`,
		},
		{
			name: "tool calls join the preceding assistant message; reasoning and non-text tool output are dropped",
			req: &v1.Request{Model: v1.ModelRefs{"m"}, Input: []v1.Item{
				&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "calling"}}},
				&v1.Reasoning{ID: "rs_1", Content: "think"},
				&v1.FunctionCall{CallID: "c1", Name: "f", Arguments: `{"a":1}`},
				&v1.FunctionCall{CallID: "c2", Name: "g", Arguments: `{}`},
				&v1.FunctionCallOutput{CallID: "c1", Content: []v1.Part{&v1.TextPart{Text: "x"}, &v1.ImagePart{ImageURL: "https://x/i.png"}, &v1.OutputTextPart{Text: "y"}}},
				&v1.FunctionCallOutput{CallID: "c2"},
				&v1.Message{Role: v1.RoleAssistant},
			}},
			wire: `{"model":"m","messages":[
				{"role":"assistant","content":"calling","tool_calls":[
					{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},
					{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"xy"},
				{"role":"tool","tool_call_id":"c2","content":""},
				{"role":"assistant","content":null}]}`,
		},
		{
			name: "mixed content becomes a part array",
			req: &v1.Request{Model: v1.ModelRefs{"m"}, Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{
				&v1.TextPart{Text: "see"},
				&v1.ImagePart{ImageURL: "https://x/i.png", Detail: "high"},
				&v1.FilePart{FileData: "QUJD", Filename: "a.txt", FileURL: "https://x/a.txt", MediaType: "text/plain"},
				&v1.FilePart{FileID: "file-1"},
				&v1.OutputTextPart{Text: "said"},
			}}}},
			wire: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"see"},
				{"type":"image_url","image_url":{"url":"https://x/i.png","detail":"high"}},
				{"type":"file","file":{"file_data":"QUJD","filename":"a.txt"}},
				{"type":"file","file":{"file_id":"file-1"}},
				{"type":"text","text":"said"}]}]}`,
		},
		{
			name: "a tool without parameters or a choice; a nil model config entry is skipped",
			req: &v1.Request{Model: v1.ModelRefs{"m"}, ModelConfig: map[string]*v1.ModelOpts{"m": nil},
				Tools: &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "f"}}}},
			wire: `{"model":"m","messages":null,"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`,
		},
		{
			name: "only the first model's options apply; an unrecognised format type passes through",
			req: &v1.Request{
				Model: v1.ModelRefs{"a", "b"},
				ModelConfig: map[string]*v1.ModelOpts{
					"a": {Sampling: &v1.SamplingParams{TopK: new(40), Temperature: floatPtr(0.1)}, Output: &v1.OutputConfig{Format: &v1.Format{Type: "grammar"}}},
					"b": {Sampling: &v1.SamplingParams{Temperature: floatPtr(0.9)}},
				},
				OutputMode: v1.OutputModeSync,
			},
			wire: `{"model":"a","messages":null,"temperature":0.1,"response_format":{"type":"grammar"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := CCTranslator{}.SerializeRequest(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, wire, tc.wire)
		})
	}
}

func TestCCSerializeRequest_Errors(t *testing.T) {
	cases := map[string]*v1.Request{
		"no model":                  {Input: []v1.Item{ccUser("x")}},
		"non-function tool":         {Model: v1.ModelRefs{"m"}, Tools: &v1.ToolsConfig{Definitions: v1.Tools{&v1.ServerTool{Name: "web_search"}}}},
		"malformed owned extension": {Model: v1.ModelRefs{"m"}, Extensions: map[string]json.RawMessage{extLogprobs: json.RawMessage(`"yes"`)}},
		"malformed format schema":   {Model: v1.ModelRefs{"m"}, ModelConfig: map[string]*v1.ModelOpts{"m": {Output: &v1.OutputConfig{Format: &v1.Format{Type: "json_schema", Schema: json.RawMessage(`{`)}}}}},
	}
	for name, req := range cases {
		if wire, err := (CCTranslator{}).SerializeRequest(req); err == nil {
			t.Errorf("%s: want an error, got %s", name, wire)
		}
	}
	var typeErr *json.UnmarshalTypeError
	if _, err := (CCTranslator{}).SerializeRequest(cases["malformed owned extension"]); !errors.As(err, &typeErr) {
		t.Errorf("malformed owned extension: %v does not wrap the decode error", err)
	}
}

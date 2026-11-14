package gemini

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func textMsg(role v1.Role, text string) *v1.Message {
	return &v1.Message{Role: role, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

// serializeWire serializes req and decodes the body's top-level members.
func serializeWire(t *testing.T, req *v1.Request) map[string]json.RawMessage {
	t.Helper()
	body, err := tr.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestSerializeRequest_Contents(t *testing.T) {
	anchor := &v1.ItemCacheConfig{Anchor: true}
	hoisted := textMsg(v1.RoleSystem, "typed queries only")
	hoisted.Hoist = true
	cases := []struct {
		name         string
		instructions string
		items        []v1.Item
		system       string // systemInstruction, "" when absent
		contents     string
	}{
		{
			name:         "leading system and developer items join systemInstruction after the instructions",
			instructions: "stable",
			items:        []v1.Item{textMsg(v1.RoleSystem, "be nice"), textMsg(v1.RoleDeveloper, "use tools"), textMsg(v1.RoleUser, "hi")},
			system:       `{"parts":[{"text":"stable"},{"text":"be nice\nuse tools"}]}`,
			contents:     `[{"role":"user","parts":[{"text":"hi"}]}]`,
		},
		{
			name:         "positional system item stays in place as a marker turn",
			instructions: "stable",
			items:        []v1.Item{textMsg(v1.RoleUser, "a"), &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "b"}}}, textMsg(v1.RoleSystem, "new rule")},
			system:       `{"parts":[{"text":"stable"}]}`,
			contents:     `[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[{"text":"b"}]},{"role":"user","parts":[{"text":"<system>\nnew rule\n</system>"}]}]`,
		},
		{
			name: "positional system item waits for the tool round to close",
			items: []v1.Item{
				textMsg(v1.RoleUser, "run it"),
				&v1.FunctionCall{CallID: "fn", Name: "fn", Arguments: `{}`},
				textMsg(v1.RoleSystem, "meanwhile"),
				&v1.FunctionCallOutput{CallID: "fn", Output: `{"ok":true}`},
			},
			contents: `[{"role":"user","parts":[{"text":"run it"}]},
				{"role":"model","parts":[{"functionCall":{"name":"fn","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"fn","response":{"ok":true}}}]},
				{"role":"user","parts":[{"text":"<system>\nmeanwhile\n</system>"}]}]`,
		},
		{
			name:     "hoisted system item moves to systemInstruction",
			items:    []v1.Item{textMsg(v1.RoleUser, "a"), textMsg(v1.RoleAssistant, "b"), hoisted},
			system:   `{"parts":[{"text":"typed queries only"}]}`,
			contents: `[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[{"text":"b"}]}]`,
		},
		{
			name: "item cache anchors leave no trace",
			items: []v1.Item{
				textMsg(v1.RoleUser, "go"),
				&v1.FunctionCall{ID: "c1", CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
				&v1.FunctionCallOutput{CallID: "c1", Output: "ok", CacheConfig: anchor},
			},
			contents: `[{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"id":"c1","name":"read","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"read","response":{"output":"ok"}}}]}]`,
		},
		{
			name:     "synthesized call id sends the bare name and no id",
			items:    []v1.Item{&v1.FunctionCallOutput{CallID: geminiCallID("search", 1), Output: `{"result":"ok"}`}},
			contents: `[{"role":"user","parts":[{"functionResponse":{"name":"search","response":{"result":"ok"}}}]}]`,
		},
		{
			// A call id minted by another vendor carries no function name.
			name: "foreign call id takes the name of its call",
			items: []v1.Item{
				&v1.FunctionCall{CallID: "toolu_01ABC", Name: "get_weather", Arguments: `{"city":"Paris"}`},
				&v1.FunctionCallOutput{CallID: "toolu_01ABC", Output: `{"temp":21}`},
			},
			contents: `[{"role":"model","parts":[{"functionCall":{"id":"toolu_01ABC","name":"get_weather","args":{"city":"Paris"}}}]},
				{"role":"user","parts":[{"functionResponse":{"id":"toolu_01ABC","name":"get_weather","response":{"temp":21}}}]}]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire := serializeWire(t, &v1.Request{
				Model:        v1.ModelRefs{"g"},
				Instructions: c.instructions,
				CacheConfig:  &v1.CacheConfig{History: true},
				Input:        c.items,
			})
			wantKeys := []string{"contents"}
			if c.system != "" {
				wantKeys = append(wantKeys, "systemInstruction")
				assertJSON(t, []byte(wire["systemInstruction"]), c.system)
			}
			if keys := slices.Sorted(maps.Keys(wire)); !slices.Equal(keys, wantKeys) {
				t.Errorf("body members %v, want %v", keys, wantKeys)
			}
			assertJSON(t, []byte(wire["contents"]), c.contents)
		})
	}
}

func TestToolChoice_RoundTrip(t *testing.T) {
	cases := []struct {
		choice v1.ToolChoice
		wire   string
		back   v1.ToolChoice
	}{
		{v1.ToolChoice{Mode: "auto"}, `{"mode":"AUTO"}`, v1.ToolChoice{Mode: "auto"}},
		{v1.ToolChoice{Mode: "required"}, `{"mode":"ANY"}`, v1.ToolChoice{Mode: "required"}},
		{v1.ToolChoice{Mode: "none"}, `{"mode":"NONE"}`, v1.ToolChoice{Mode: "none"}},
		{v1.ToolChoice{Mode: "function", FunctionName: "f"}, `{"mode":"ANY","allowedFunctionNames":["f"]}`, v1.ToolChoice{Mode: "function", FunctionName: "f"}},
		{v1.ToolChoice{Mode: "unknown"}, `{"mode":"AUTO"}`, v1.ToolChoice{Mode: "auto"}},
	}
	for _, c := range cases {
		t.Run(c.choice.Mode, func(t *testing.T) {
			req := &v1.Request{Tools: &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "f"}}, Choice: &c.choice}}
			body, err := tr.SerializeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Tools      json.RawMessage
				ToolConfig struct{ FunctionCallingConfig json.RawMessage }
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			assertJSON(t, []byte(wire.Tools), `[{"functionDeclarations":[{"name":"f","parameters":{}}]}]`)
			assertJSON(t, []byte(wire.ToolConfig.FunctionCallingConfig), c.wire)
			if back := parseRequest(t, string(body)).Tools; back == nil || !reflect.DeepEqual(back.Choice, &c.back) {
				t.Errorf("parsed back %+v, want choice %+v", back, c.back)
			}
		})
	}
}

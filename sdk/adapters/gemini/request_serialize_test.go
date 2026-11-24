package gemini

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
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
				{"role":"model","parts":[{"functionCall":{"name":"fn","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]},
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
				{"role":"model","parts":[{"functionCall":{"id":"c1","name":"read","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]},
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
			contents: `[{"role":"model","parts":[{"functionCall":{"id":"toolu_01ABC","name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"skip_thought_signature_validator"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"toolu_01ABC","name":"get_weather","response":{"temp":21}}}]}]`,
		},
		{
			// Gemini rejects a part with no data field, so a signature-only part keeps its empty text.
			name: "thought signatures ride the parts they came on",
			items: []v1.Item{
				&v1.Reasoning{Content: "plan", ProviderData: thoughtSignatureJSON("s1")},
				&v1.Reasoning{ProviderData: thoughtSignatureJSON("s2")},
				&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "a"}, &v1.OutputTextPart{Text: "b"}}, ProviderData: thoughtSignatureJSON("s3")},
				&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "c"}}, ProviderData: json.RawMessage(`{"emptyTextPart":true,"thoughtSignature":"s4"}`)},
				&v1.Reasoning{ProviderData: json.RawMessage(`{"emptyTextPart":true,"thoughtSignature":"s5"}`)},
				&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "d"}}, ProviderData: json.RawMessage(`{"type":"thinking","signature":"other vendor"}`)},
			},
			contents: `[{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"s1"}]},
				{"role":"model","parts":[{"text":"","thought":true,"thoughtSignature":"s2"}]},
				{"role":"model","parts":[{"text":"a"},{"text":"b","thoughtSignature":"s3"}]},
				{"role":"model","parts":[{"text":"c"},{"text":"","thoughtSignature":"s4"}]},
				{"role":"model","parts":[{"text":"","thoughtSignature":"s5"}]},
				{"role":"model","parts":[{"text":"d"}]}]`,
		},
		{
			name: "first call of each step without a Gemini signature gets the skip signature, parallel calls stay bare",
			items: []v1.Item{
				textMsg(v1.RoleUser, "go"),
				&v1.FunctionCall{CallID: "a", Name: "a", Arguments: `{}`},
				&v1.FunctionCall{CallID: "b", Name: "b", Arguments: `{}`},
				&v1.FunctionCallOutput{CallID: "a", Output: "1"},
				&v1.FunctionCallOutput{CallID: "b", Output: "2"},
				&v1.FunctionCall{CallID: "c", Name: "c", Arguments: `{}`, ProviderData: json.RawMessage(`{"type":"thinking","signature":"other vendor"}`)},
				&v1.FunctionCallOutput{CallID: "c", Output: "3"},
			},
			contents: `[{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"name":"a","args":{}},"thoughtSignature":"skip_thought_signature_validator"},{"functionCall":{"name":"b","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"a","response":{"output":1}}},{"functionResponse":{"name":"b","response":{"output":2}}}]},
				{"role":"model","parts":[{"functionCall":{"name":"c","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]},
				{"role":"user","parts":[{"functionResponse":{"name":"c","response":{"output":3}}}]}]`,
		},
		{
			name: "a real call signature is kept",
			items: []v1.Item{
				textMsg(v1.RoleUser, "go"),
				&v1.FunctionCall{CallID: "a", Name: "a", Arguments: `{}`, ProviderData: thoughtSignatureJSON("sig-a")},
				&v1.FunctionCall{CallID: "b", Name: "b", Arguments: `{}`},
				&v1.FunctionCallOutput{CallID: "a", Output: "1"},
				&v1.FunctionCallOutput{CallID: "b", Output: "2"},
			},
			contents: `[{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"name":"a","args":{}},"thoughtSignature":"sig-a"},{"functionCall":{"name":"b","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"a","response":{"output":1}}},{"functionResponse":{"name":"b","response":{"output":2}}}]}]`,
		},
		{
			name:     "system item between turns keeps its place, one after a tool round follows the responses",
			items:    []v1.Item{textMsg(v1.RoleUser, "a"), textMsg(v1.RoleSystem, "s1"), textMsg(v1.RoleUser, "b"), &v1.FunctionCall{CallID: "f", Name: "f", Arguments: `{}`}, &v1.FunctionCallOutput{CallID: "f", Output: `[1,2]`}, textMsg(v1.RoleSystem, "s2"), textMsg(v1.RoleUser, "c")},
			contents: `[{"role":"user","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":"<system>\ns1\n</system>"}]},{"role":"user","parts":[{"text":"b"}]},{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":[1,2]}}}]},{"role":"user","parts":[{"text":"<system>\ns2\n</system>"}]},{"role":"user","parts":[{"text":"c"}]}]`,
		},
		{
			name:     "trailing call without arguments, then a system item",
			items:    []v1.Item{textMsg(v1.RoleUser, "go"), &v1.FunctionCall{CallID: "c1", Name: "f"}, textMsg(v1.RoleSystem, "late")},
			contents: `[{"role":"user","parts":[{"text":"go"}]},{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]},{"role":"user","parts":[{"text":"<system>\nlate\n</system>"}]}]`,
		},
		{
			name:     "tool result takes its content text only when the output is empty",
			items:    []v1.Item{&v1.FunctionCallOutput{CallID: "f", Content: []v1.Part{&v1.TextPart{Text: "a"}, &v1.TextPart{Text: "b"}}}, &v1.FunctionCallOutput{CallID: "f", Output: "x", Content: []v1.Part{&v1.TextPart{Text: "y"}}}, &v1.FunctionCallOutput{CallID: "f"}},
			contents: `[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"ab"}}},{"functionResponse":{"name":"f","response":{"output":"x"}}},{"functionResponse":{"name":"f","response":{"output":""}}}]}]`,
		},
		{
			name:     "reasoning sends its content, else its first summary, else nothing, and opens the conversation; empty system item dropped",
			items:    []v1.Item{&v1.Reasoning{Content: "c", Summary: []v1.SummaryText{{Text: "s"}}}, textMsg(v1.RoleSystem, "rule"), &v1.Reasoning{Summary: []v1.SummaryText{{Text: "s"}}}, &v1.Reasoning{}, textMsg(v1.RoleSystem, ""), textMsg(v1.RoleAssistant, "a")},
			contents: `[{"role":"model","parts":[{"text":"c","thought":true}]},{"role":"user","parts":[{"text":"<system>\nrule\n</system>"}]},{"role":"model","parts":[{"text":"s","thought":true}]},{"role":"model","parts":[{"text":"a"}]}]`,
		},
		{
			name:     "file data without a media type, image URL holding data URL separators",
			items:    []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.FilePart{FileData: "QUJD"}, &v1.ImagePart{ImageURL: "https://example.com/i;v=1,2.png"}}}},
			contents: `[{"role":"user","parts":[{"inlineData":{"mimeType":"application/octet-stream","data":"QUJD"}},{"fileData":{"fileUri":"https://example.com/i;v=1,2.png"}}]}]`,
		},
		{
			name:     "image data URL without base64 goes inline re-encoded",
			items:    []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.ImagePart{ImageURL: "data:image/png,abc"}}}},
			contents: `[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"YWJj"}}]}]`,
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

func TestSerializeRequest_Config(t *testing.T) {
	temperature, budget := 0.5, 512
	perModel := map[string]*v1.ModelOpts{
		"g":     {Sampling: &v1.SamplingParams{Temperature: &temperature}},
		"other": {Reasoning: &v1.ReasoningConfig{BudgetTokens: &budget}},
	}
	only := func(o v1.ModelOpts) map[string]*v1.ModelOpts { return map[string]*v1.ModelOpts{"g": &o} }
	cases := []struct {
		name string
		req  v1.Request
		want string
	}{
		{"options of the request model", v1.Request{Model: v1.ModelRefs{"g"}, ModelConfig: perModel}, `{"generationConfig":{"temperature":0.5}}`},
		{"model outside a multi-entry config gets none", v1.Request{Model: v1.ModelRefs{"x"}, ModelConfig: perModel}, `{}`},
		{"empty options", v1.Request{ModelConfig: only(v1.ModelOpts{})}, `{}`},
		{"reasoning without a budget", v1.Request{ModelConfig: only(v1.ModelOpts{Reasoning: &v1.ReasoningConfig{}})}, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`},
		{"json_object alone", v1.Request{ModelConfig: only(v1.ModelOpts{Output: &v1.OutputConfig{Format: &v1.Format{Type: "json_object"}}})}, `{"generationConfig":{"responseMimeType":"application/json"}}`},
		{"text format alone", v1.Request{ModelConfig: only(v1.ModelOpts{Output: &v1.OutputConfig{Format: &v1.Format{Type: "text"}}})}, `{}`},
		// The OpenAPI-subset fields (responseSchema, parameters) reject JSON Schema keywords such as additionalProperties.
		{
			"json_schema and tool parameters go out as JSON Schema",
			v1.Request{
				ModelConfig: only(v1.ModelOpts{Output: &v1.OutputConfig{Format: &v1.Format{Type: "json_schema", Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}}}),
				Tools:       &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "f", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}}},
			},
			`{"generationConfig":{"responseMimeType":"application/json","responseJsonSchema":{"type":"object","additionalProperties":false}},
			  "tools":[{"functionDeclarations":[{"name":"f","parametersJsonSchema":{"type":"object","additionalProperties":false}}]}]}`,
		},
		{"choice without definitions", v1.Request{Tools: &v1.ToolsConfig{Choice: &v1.ToolChoice{Mode: "auto"}}}, `{"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.Input = []v1.Item{textMsg(v1.RoleUser, "hi")}
			wire := serializeWire(t, &c.req)
			delete(wire, "contents")
			assertJSON(t, wire, c.want)
		})
	}
}

func TestSerializeRequest_RejectsUnknownFormatType(t *testing.T) {
	req := &v1.Request{Input: []v1.Item{textMsg(v1.RoleUser, "hi")}, ModelConfig: map[string]*v1.ModelOpts{"g": {Output: &v1.OutputConfig{Format: &v1.Format{Type: "grammar"}}}}}
	if _, err := tr.SerializeRequest(req); !errors.Is(err, v1.ErrUnsupportedRequest) {
		t.Errorf("err = %v, want ErrUnsupportedRequest", err)
	}
}

func TestSerializeRequest_RejectsNoContents(t *testing.T) {
	req := &v1.Request{Instructions: "be brief", Input: []v1.Item{textMsg(v1.RoleSystem, "and kind")}}
	_, err := tr.SerializeRequest(req)
	if err == nil || err.Error() != "gemini serialize_request: not supported by the upstream API: no contents to send" || !errors.Is(err, v1.ErrUnsupportedRequest) {
		t.Errorf("err = %v, want an unsupported-request error", err)
	}
}

func TestSerializeRequest_UnsendableImageOrFilePart(t *testing.T) {
	for name, part := range map[string]v1.Part{"file without data or URL": &v1.FilePart{}, "image data URL with a bad percent escape": &v1.ImagePart{ImageURL: "data:image/png,%zz"}, "image data URL without a comma": &v1.ImagePart{ImageURL: "data:image/png;base64"}} {
		t.Run(name, func(t *testing.T) {
			req := &v1.Request{Input: []v1.Item{&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{part}}}}
			if body, err := tr.SerializeRequest(req); err == nil {
				t.Errorf("serialized to %s, want an error", body)
			}
		})
	}
}

// Vertex rejects a fileData without a mimeType: the variant sends the part's media type, else the one its URL path's extension names, and refuses a part with neither; the Gemini API form sends what the part holds.
func TestSerializeRequest_VertexFileMediaType(t *testing.T) {
	cases := []struct {
		name         string
		part         v1.Part
		vertex, gapi string // the fileData each form sends, "" when the variant refuses the part
	}{
		{"image URL with a known extension", &v1.ImagePart{ImageURL: "https://example.com/a/cat.JPG?sig=x.pdf#y"},
			`{"mimeType":"image/jpeg","fileUri":"https://example.com/a/cat.JPG?sig=x.pdf#y"}`, `{"fileUri":"https://example.com/a/cat.JPG?sig=x.pdf#y"}`},
		{"image URL without one", &v1.ImagePart{ImageURL: "https://example.com/cat"}, "", `{"fileUri":"https://example.com/cat"}`},
		{"file URL with a media type", &v1.FilePart{FileURL: "https://example.com/clip", MediaType: "video/mp4"},
			`{"mimeType":"video/mp4","fileUri":"https://example.com/clip"}`, `{"mimeType":"video/mp4","fileUri":"https://example.com/clip"}`},
		{"file URL whose media type outranks its extension", &v1.FilePart{FileURL: "gs://b/a.pdf", MediaType: "text/plain"},
			`{"mimeType":"text/plain","fileUri":"gs://b/a.pdf"}`, `{"mimeType":"text/plain","fileUri":"gs://b/a.pdf"}`},
		{"gs file URL with a known extension", &v1.FilePart{FileURL: "gs://b/talk.mp3"}, `{"mimeType":"audio/mp3","fileUri":"gs://b/talk.mp3"}`, `{"fileUri":"gs://b/talk.mp3"}`},
		{"file URL with an unknown extension", &v1.FilePart{FileURL: "https://example.com/notes.txt"}, "", `{"fileUri":"https://example.com/notes.txt"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &v1.Request{Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{c.part}}}}
			for _, form := range []struct {
				tr   GeminiTranslator
				want string
			}{{GeminiTranslator{Vertex: true}, c.vertex}, {GeminiTranslator{}, c.gapi}} {
				body, err := form.tr.SerializeRequest(req)
				if form.want == "" {
					if !errors.Is(err, v1.ErrUnsupportedRequest) {
						t.Errorf("Vertex: serialized to %s (err %v), want ErrUnsupportedRequest", body, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("Vertex=%v: %v", form.tr.Vertex, err)
				}
				assertJSON(t, body, `{"contents":[{"role":"user","parts":[{"fileData":`+form.want+`}]}]}`)
			}
		})
	}
}

func TestToolChoice_RoundTrip(t *testing.T) {
	const fAndG = `[{"functionDeclarations":[{"name":"f","parametersJsonSchema":{}},{"name":"g","parametersJsonSchema":{}}]}]`
	cases := []struct {
		choice v1.ToolChoice
		wire   string
		back   v1.ToolChoice
		// tools is the declarations sent; empty means both f and g.
		tools string
	}{
		{v1.ToolChoice{Mode: "auto"}, `{"mode":"AUTO"}`, v1.ToolChoice{Mode: "auto"}, ""},
		{v1.ToolChoice{Mode: "required"}, `{"mode":"ANY"}`, v1.ToolChoice{Mode: "required"}, ""},
		{v1.ToolChoice{Mode: "none"}, `{"mode":"NONE"}`, v1.ToolChoice{Mode: "none"}, ""},
		{v1.ToolChoice{Mode: "function", FunctionName: "f"}, `{"mode":"ANY","allowedFunctionNames":["f"]}`, v1.ToolChoice{Mode: "function", FunctionName: "f"}, ""},
		{v1.ToolChoice{Mode: "unknown"}, `{"mode":"AUTO"}`, v1.ToolChoice{Mode: "auto"}, ""},
		{v1.ToolChoice{Mode: "required", Allowed: []string{"g", "f"}}, `{"mode":"ANY","allowedFunctionNames":["g","f"]}`, v1.ToolChoice{Mode: "required", Allowed: []string{"g", "f"}}, ""},
		// AUTO takes no allowedFunctionNames, so only the allowed declaration is sent.
		{v1.ToolChoice{Mode: "auto", Allowed: []string{"g"}}, `{"mode":"AUTO"}`, v1.ToolChoice{Mode: "auto"}, `[{"functionDeclarations":[{"name":"g","parametersJsonSchema":{}}]}]`},
		{v1.ToolChoice{Mode: "function"}, `{"mode":"ANY"}`, v1.ToolChoice{Mode: "required"}, ""},
	}
	for _, c := range cases {
		t.Run(c.choice.Mode+strings.Join(c.choice.Allowed, ","), func(t *testing.T) {
			req := &v1.Request{Input: []v1.Item{textMsg(v1.RoleUser, "hi")}, Tools: &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "f"}, &v1.FunctionTool{Name: "g"}}, Choice: &c.choice}}
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
			tools := c.tools
			if tools == "" {
				tools = fAndG
			}
			assertJSON(t, []byte(wire.Tools), tools)
			assertJSON(t, []byte(wire.ToolConfig.FunctionCallingConfig), c.wire)
			if back := parseRequest(t, string(body)).Tools; back == nil || !reflect.DeepEqual(back.Choice, &c.back) {
				t.Errorf("parsed back %+v, want choice %+v", back, c.back)
			}
		})
	}
}

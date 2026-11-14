package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

const respHiInput = `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`

func TestResponsesParseRequest(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "sampling, reasoning, format and stream",
			body: `{"model":"gpt-5","input":"hi","temperature":0.8,"top_p":0.95,"max_output_tokens":1024,"stop_sequences":["END"],"stream":true,"user":"u1","metadata":{"team":"core"},
				"reasoning":{"effort":"high","summary":"detailed"},
				"text":{"verbosity":"low","format":{"type":"json_schema","name":"S","description":"a schema","schema":{"type":"object"},"strict":true}}}`,
			want: `{"model":"gpt-5","input":` + respHiInput + `,"model_config":{"gpt-5":{
				"sampling":{"temperature":0.8,"top_p":0.95,"max_tokens":1024,"stop":["END"]},
				"reasoning":{"effort":"high","summary":"detailed"},
				"output":{"format":{"type":"json_schema","name":"S","description":"a schema","schema":{"type":"object"},"strict":true},"verbosity":"low"}}},
				"output_mode":"stream","user":"u1","metadata":{"team":"core"}}`,
		},
		{
			name: "cache key, 24h retention and service tier; include and false stateful flags accepted",
			body: `{"model":"gpt-5","input":"hi","prompt_cache_key":"conv-1","prompt_cache_retention":"24h","service_tier":"priority",
				"include":["reasoning.encrypted_content","file_search_call.results"],"store":false,"background":false,"context_management":null,"prompt":null}`,
			want: `{"model":"gpt-5","input":` + respHiInput + `,"cache_config":{"key":"conv-1","ttl":"24h"},"output_mode":"sync","extensions":{"openai.service_tier":"priority"}}`,
		},
		{
			name: "unknown cache retention",
			body: `{"model":"gpt-5","input":"hi","prompt_cache_retention":"forever"}`,
			want: `{"model":"gpt-5","input":` + respHiInput + `,"output_mode":"sync"}`,
		},
		{
			// Hosted tools and an mcp tool MCPTool cannot hold drop; namespaced tools flatten to their own names.
			name: "tool definitions",
			body: `{"model":"gpt-5","input":"hi","parallel_tool_calls":false,"tool_choice":{"type":"file_search"},"tools":[
				{"type":"web_search"},
				{"type":"function","name":"f"},
				{"type":"namespace","name":"ns","description":"","tools":[{"type":"function","name":"a","description":"A","parameters":{"type":"object"},"strict":true},{"type":"web_search"}]},
				{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.com","headers":{"X-Key":"k"}},
				{"type":"mcp","server_label":"gated","server_url":"https://mcp.example.com","require_approval":"never"}]}`,
			want: `{"model":"gpt-5","input":` + respHiInput + `,"tools":{"definitions":[
				{"type":"function","name":"f","parameters":{}},
				{"type":"function","name":"a","description":"A","parameters":{"type":"object"},"strict":true},
				{"type":"mcp","name":"docs","server_url":"https://mcp.example.com","headers":{"X-Key":"k"}}],
				"choice":"file_search","parallel":false},"output_mode":"sync"}`,
		},
		{
			name: "function tool choice",
			body: `{"model":"gpt-5","input":"hi","tools":[{"type":"function","name":"f","parameters":{}}],"tool_choice":{"type":"function","name":"f"}}`,
			want: `{"model":"gpt-5","input":` + respHiInput + `,"tools":{"definitions":[{"type":"function","name":"f","parameters":{}}],"choice":{"type":"function","name":"f"}},"output_mode":"sync"}`,
		},
		{
			// A hosted-tool item has no canonical form and drops; reasoning content parts concatenate as the stream's deltas do.
			name: "input items",
			body: `{"model":"gpt-5","input":[
				{"type":"web_search_call","id":"ws_0","status":"completed"},
				{"type":"message","role":"developer","content":"look"},
				{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/cat.png","detail":"high"},{"type":"input_file","file_id":"file-1","filename":"a.pdf"}]},
				{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"S"}],"content":[{"type":"reasoning_text","text":"221 = "},{"type":"reasoning_text","text":"13 * 17"}],"encrypted_content":"BLOB"},
				{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{}","status":"completed"},
				{"type":"function_call_output","call_id":"c1","output":""}]}`,
			want: `{"model":"gpt-5","input":[
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"look"}]},
				{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/cat.png","detail":"high"},{"type":"input_file","file_id":"file-1","filename":"a.pdf"}]},
				{"type":"reasoning","id":"rs_1","summary":[{"text":"S"}],"content":"221 = 13 * 17","provider_data":{"encrypted_content":"BLOB","id":"rs_1"}},
				{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{}","status":"completed"},
				{"type":"function_call_output","call_id":"c1"}],"output_mode":"sync"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			respJSONEq(t, respParseRequest(t, tc.body), tc.want)
		})
	}
}

func TestResponsesParseRequest_Rejected(t *testing.T) {
	const audioInput = `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"listen"},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}]}]}`
	stateful := func(field string) string {
		return `responses_unsupported_canonical: field "` + field + `" has no canonical equivalent`
	}
	cases := []struct {
		name, body, wantErr string
	}{
		{"previous_response_id", `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, stateful("previous_response_id")},
		{"store", `{"model":"m","input":"hi","store":true}`, stateful("store")},
		{"conversation", `{"model":"m","input":"hi","conversation":"conv_1"}`, stateful("conversation")},
		{"background", `{"model":"m","input":"hi","background":true}`, stateful("background")},
		{"truncation", `{"model":"m","input":"hi","truncation":"auto"}`, stateful("truncation")},
		{"safety_identifier", `{"model":"m","input":"hi","safety_identifier":"s"}`, stateful("safety_identifier")},
		{"context_management", `{"model":"m","input":"hi","context_management":{"type":"auto"}}`, stateful("context_management")},
		{"stored prompt", `{"model":"m","input":"hi","prompt":{"id":"pmpt_1","version":"2"}}`, stateful("prompt")},
		{"unmodeled input part", audioInput, `input item: unsupported part type "input_audio"`},
		{"no model", `{"input":"hi"}`, "responses parse_request: model is required"},
		{"no input", `{"model":"m"}`, "responses parse_request: input is required"},
		{"not JSON", `{"model":`, "responses parse_request: invalid JSON: unexpected end of JSON input"},
		{"metadata entries", `{"model":"m","input":"hi","metadata":{` + respMetadataEntries(17) + `}}`, "responses parse_request: metadata: too many entries: 17 (max 16)"},
		{"metadata key charset", `{"model":"m","input":"hi","metadata":{"a b":"v"}}`, `responses parse_request: metadata: key "a b" contains invalid characters (DNS-1123 charset required)`},
		{"metadata key length", `{"model":"m","input":"hi","metadata":{"` + strings.Repeat("k", 65) + `":"v"}}`, `responses parse_request: metadata: key "` + strings.Repeat("k", 65) + `" exceeds 64 characters`},
		{"metadata value length", `{"model":"m","input":"hi","metadata":{"k":"` + strings.Repeat("v", 257) + `"}}`, `responses parse_request: metadata: value for key "k" exceeds 256 characters`},
		{"metadata value charset", `{"model":"m","input":"hi","metadata":{"k":"a\nb"}}`, `responses parse_request: metadata: value for key "k" contains non-printable or disallowed ASCII characters`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResponsesTranslator{}.ParseRequest([]byte(tc.body))
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	// The same-shape byte-pass path decodes the wire without canonical, so it must still accept what canonical refuses.
	if _, err := ParseResponsesRequest([]byte(audioInput)); err != nil {
		t.Errorf("ParseResponsesRequest refused an unmodeled part: %v", err)
	}
}

func respMetadataEntries(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = `"k` + string(rune('a'+i)) + `":"v"`
	}
	return strings.Join(entries, ",")
}

func TestResponsesSerializeRequest(t *testing.T) {
	const tail = `"store":false,"include":["reasoning.encrypted_content"]`
	cases := []struct {
		name string
		req  *v1.Request
		want string
	}{
		{
			// Responses has no stop_sequences, top_k, seed, penalties or reasoning budget: emitting any of them 400s.
			name: "sampling and request options",
			req: &v1.Request{
				Model: v1.ModelRefs{"gpt-5"}, Input: []v1.Item{respUserText("hi")}, OutputMode: v1.OutputModeStream, User: "u1",
				Metadata:    map[string]string{"team": "core"},
				CacheConfig: &v1.CacheConfig{Key: "conv-1", TTL: "5m", History: true},
				Extensions:  map[string]json.RawMessage{extServiceTier: json.RawMessage(`"flex"`), extLogitBias: json.RawMessage(`{"1":2}`)},
				ModelConfig: map[string]*v1.ModelOpts{"gpt-5": {
					Sampling:  &v1.SamplingParams{Temperature: respPtr(0.2), TopP: respPtr(0.9), TopK: respPtr(40), MaxTokens: respPtr(64), Stop: []string{"END"}, Seed: respPtr(7)},
					Reasoning: &v1.ReasoningConfig{Effort: "low", BudgetTokens: respPtr(100)},
				}},
			},
			want: `{"model":"gpt-5","input":` + respHiInput + `,"temperature":0.2,"top_p":0.9,"max_output_tokens":64,"reasoning":{"effort":"low"},
				"user":"u1","metadata":{"team":"core"},"stream":true,` + tail + `,"prompt_cache_key":"conv-1","prompt_cache_retention":"in_memory","service_tier":"flex"}`,
		},
		{
			// The API rejects input ids it did not mint and output-only status; foreign reasoning is provider-signed and cannot cross vendors.
			name: "input item ids and status",
			req: &v1.Request{Model: v1.ModelRefs{"gpt-5"}, Input: []v1.Item{
				&v1.Message{ID: "m_0", Status: v1.StatusCompleted, Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
				&v1.Reasoning{ID: "r_0", Content: "thinking", Status: v1.StatusCompleted, ProviderData: json.RawMessage(`{"type":"thinking","signature":"anthropic-signed"}`)},
				&v1.FunctionCall{ID: "t_0", CallID: "toolu_01", Name: "search", Arguments: `{"q":"x"}`, Status: v1.StatusCompleted},
				&v1.FunctionCallOutput{CallID: "toolu_01", Output: "result"},
				&v1.Reasoning{ID: "rs_1", Content: "raw", Status: v1.StatusCompleted, ProviderData: json.RawMessage(`{"encrypted_content":"BLOB"}`)},
				&v1.FunctionCall{ID: "fc_1", CallID: "c1", Name: "search", Arguments: `{}`, Status: v1.StatusCompleted},
				&v1.Message{ID: "msg_1", Status: v1.StatusCompleted, Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "done"}}},
			}},
			want: `{"model":"gpt-5","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"function_call","call_id":"toolu_01","name":"search","arguments":"{\"q\":\"x\"}"},
				{"type":"function_call_output","call_id":"toolu_01","output":"result"},
				{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"raw"}],"encrypted_content":"BLOB"},
				{"type":"function_call","id":"fc_1","call_id":"c1","name":"search","arguments":"{}"},
				{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[],"logprobs":[]}]}],` + tail + `}`,
		},
		{
			// The API ties the text part type to the role, not to the canonical part variant.
			name: "text part type follows role",
			req: &v1.Request{Model: v1.ModelRefs{"gpt-5"}, Input: []v1.Item{
				&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{
					&v1.TextPart{Text: "a"},
					&v1.OutputTextPart{Text: "b", Annotations: []v1.Annotation{&v1.URLCitationAnnotation{StartIndex: 0, EndIndex: 1, URL: "https://x", Title: "X"}, &v1.TextCitationAnnotation{StartIndex: 0, EndIndex: 1}}},
				}},
				&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.OutputTextPart{Text: "c"}, &v1.FilePart{FileData: "JVBE", Filename: "a.pdf", MediaType: "application/pdf"}}},
				&v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: "d"}}},
			}},
			want: `{"model":"gpt-5","input":[
				{"type":"message","role":"assistant","content":[
					{"type":"output_text","text":"a","annotations":[],"logprobs":[]},
					{"type":"output_text","text":"b","annotations":[{"type":"url_citation","start_index":0,"end_index":1,"url":"https://x","title":"X"}],"logprobs":[]}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"c"},{"type":"input_file","file_data":"JVBE","filename":"a.pdf"}]},
				{"type":"message","role":"system","content":[{"type":"input_text","text":"d"}]}],` + tail + `}`,
		},
		{
			name: "tools and hosted tool choice",
			req: &v1.Request{Model: v1.ModelRefs{"gpt-5"}, Input: []v1.Item{respUserText("hi")}, CacheConfig: &v1.CacheConfig{Key: "k"},
				Tools: &v1.ToolsConfig{
					Definitions: v1.Tools{
						&v1.FunctionTool{Name: "f", ProviderData: json.RawMessage(`{"foreign":true}`)},
						&v1.MCPTool{Name: "docs", ServerURL: "https://mcp.example.com", Headers: map[string]string{"X-Key": "k"}},
					},
					Choice:   &v1.ToolChoice{Mode: "file_search"},
					Parallel: respPtr(true),
				}},
			want: `{"model":"gpt-5","input":` + respHiInput + `,"tools":[
				{"type":"function","name":"f","parameters":{}},
				{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.com","headers":{"X-Key":"k"}}],
				"tool_choice":{"type":"file_search"},"parallel_tool_calls":true,` + tail + `,"prompt_cache_key":"k"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			respJSONEq(t, respSerializeRequest(t, tc.req), tc.want)
		})
	}
}

func TestResponsesSerializeRequest_Rejected(t *testing.T) {
	withTool := func(tool v1.Tool) *v1.Request {
		return &v1.Request{Model: v1.ModelRefs{"m"}, Input: []v1.Item{respUserText("hi")}, Tools: &v1.ToolsConfig{Definitions: v1.Tools{tool}}}
	}
	cases := map[string]struct {
		req     *v1.Request
		wantErr string
	}{
		"no model":         {&v1.Request{Input: []v1.Item{respUserText("hi")}}, "canonical request has no model"},
		"name-only mcp":    {withTool(&v1.MCPTool{Name: "docs"}), "responses serialize_request: mcp tool needs both name and server_url"},
		"relay-run tool":   {withTool(&v1.ServerTool{Name: "web_search"}), "responses serialize_request: unsupported tool type *v1.ServerTool"},
		"bad service tier": {&v1.Request{Model: v1.ModelRefs{"m"}, Extensions: map[string]json.RawMessage{extServiceTier: json.RawMessage(`5`)}}, `extensions["openai.service_tier"]: json: cannot unmarshal number into Go value of type string`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ResponsesTranslator{}.SerializeRequest(tc.req)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// A prior response's output is the next turn's input: reasoning must keep its id and blob so the function_call keeps its required sibling, and no item may carry the output-only status.
func TestResponsesResponseOutputReplaysAsInput(t *testing.T) {
	resp := respParseResponse(t, `{"id":"r","object":"response","status":"completed","model":"m","output":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"BLOB","summary":[],"status":"completed"},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{}","status":"completed"},
		{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]}`)
	got := respSerializeRequest(t, &v1.Request{Model: v1.ModelRefs{"m"}, Input: resp.Output})
	respJSONEq(t, respMember(t, got, "input"), `[
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"BLOB"},
		{"type":"function_call","id":"fc_1","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[],"logprobs":[]}]}]`)
}

func TestResponsesParseRequest_TopK(t *testing.T) {
	req := respParseRequest(t, `{"model":"m","input":"hi","top_k":40}`)
	respJSONEq(t, req.ModelConfig["m"].Sampling, `{"top_k":40}`)
}

// The Responses API types function_call_output.output as a string or an array of input_text/input_image/input_file parts; there is no content member.
func TestResponsesParseRequest_FunctionCallOutputArray(t *testing.T) {
	req := respParseRequest(t, `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)
	respJSONEq(t, req.Input, `[{"type":"function_call_output","call_id":"c","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]`)
}

func TestResponsesSerializeRequest_FunctionCallOutputArray(t *testing.T) {
	got := respSerializeRequest(t, &v1.Request{Model: v1.ModelRefs{"m"}, Input: []v1.Item{
		&v1.FunctionCallOutput{CallID: "c", Content: []v1.Part{&v1.ImagePart{ImageURL: "data:image/png;base64,AAAA"}}},
	}})
	respJSONEq(t, respMember(t, got, "input"), `[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]`)
}

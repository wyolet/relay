package gemini

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var tr GeminiTranslator

func parseRequest(t *testing.T, body string) *v1.Request {
	t.Helper()
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// assertJSON fails unless got (a value, or raw JSON bytes) and want are the
// same JSON document, key order and whitespace aside.
func assertJSON(t *testing.T, got any, want string) {
	t.Helper()
	g, ok := got.([]byte)
	if !ok {
		var err error
		if g, err = json.Marshal(got); err != nil {
			t.Fatal(err)
		}
	}
	if diff := cmp.Diff(decodeJSON(t, []byte(want)), decodeJSON(t, g)); diff != "" {
		t.Errorf("JSON mismatch (-want +got):\n%s", diff)
	}
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", b, err)
	}
	return v
}

func TestParseRequest_Contents(t *testing.T) {
	marker, _ := json.Marshal(v1.WrapSystemMarker("rule"))
	quoted, _ := json.Marshal("quote: " + v1.WrapSystemMarker("x"))
	cases := []struct{ name, contents, want string }{
		{
			"roles, unknown read as user, empty text skipped",
			`[{"role":"user","parts":[{"text":"ping"},{"text":""}]},{"role":"model","parts":[{"text":"pong"}]},{"role":"tool","parts":[{"text":"again"}]}]`,
			`[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]},
			  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]},
			  {"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]`,
		},
		{
			"whole-turn system marker restores the system role",
			`[{"role":"user","parts":[{"text":` + string(marker) + `}]}]`,
			`[{"type":"message","role":"system","content":[{"type":"input_text","text":"rule"}]}]`,
		},
		{
			"embedded system marker stays user text",
			`[{"role":"user","parts":[{"text":` + string(quoted) + `}]}]`,
			`[{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(quoted) + `}]}]`,
		},
		{
			"call without args or id, response without body",
			`[{"role":"model","parts":[{"functionCall":{"name":"f"}}]},{"role":"user","parts":[{"functionResponse":{"name":"f"}}]}]`,
			`[{"type":"function_call","call_id":"f","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"f"}]`,
		},
		{
			// ImagePart has no media type for a URL, so an image fileData stays a FilePart to keep its mimeType.
			"image fileData stays a file part",
			`[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"https://example.com/x"}}]}]`,
			`[{"type":"message","role":"user","content":[{"type":"input_file","file_url":"https://example.com/x","media_type":"image/png"}]}]`,
		},
		{
			"proto field names on parts",
			`[{"role":"model","parts":[{"function_call":{"id":"c1","name":"f","args":{"a":1}},"thought_signature":"sig"}]},
			  {"role":"user","parts":[{"function_response":{"id":"c1","name":"f","response":{"ok":true}}},{"file_data":{"mime_type":"application/pdf","file_uri":"https://example.com/x"}}]}]`,
			`[{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}","provider_data":{"thoughtSignature":"sig"}},
			  {"type":"function_call_output","call_id":"c1","output":"{\"ok\":true}"},
			  {"type":"message","role":"user","content":[{"type":"input_file","file_url":"https://example.com/x","media_type":"application/pdf"}]}]`,
		},
		{
			// A signed part ends its message, so the signature stays on that part.
			"signatures on thought, text and empty-text parts",
			`[{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"s1"},{"text":"","thought":true,"thoughtSignature":"s2"},{"text":"a"},{"text":"b","thoughtSignature":"s3"},{"text":"c"},{"text":"","thoughtSignature":"s4"},{"text":"","thoughtSignature":"s5"}]}]`,
			`[{"type":"reasoning","summary":[{"text":"plan"}],"content":"plan","provider_data":{"thoughtSignature":"s1"}},
			  {"type":"reasoning","provider_data":{"thoughtSignature":"s2"}},
			  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}],"provider_data":{"thoughtSignature":"s3"}},
			  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"c"}],"provider_data":{"emptyTextPart":true,"thoughtSignature":"s4"}},
			  {"type":"reasoning","provider_data":{"emptyTextPart":true,"thoughtSignature":"s5"}}]`,
		},
		{
			"marker beside other parts stays user text, empty turn skipped",
			`[{"role":"user","parts":[{"text":` + string(marker) + `},{"text":"more"}]},{"role":"user","parts":[]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}},{"text":` + string(marker) + `}]}]`,
			`[{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(marker) + `},{"type":"input_text","text":"more"}]},{"type":"function_call_output","call_id":"f","output":"{\"ok\":true}"},{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(marker) + `}]}]`,
		},
		{
			"model thoughts read as reasoning, empty text skipped",
			`[{"role":"model","parts":[{"text":"plan","thought":true},{"text":""},{"text":"more","thought":true,"thoughtSignature":"sig"},{"text":"done"}]}]`,
			`[{"type":"reasoning","summary":[{"text":"plan"}],"content":"plan"},{"type":"reasoning","summary":[{"text":"more"}],"content":"more","provider_data":{"thoughtSignature":"sig"}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertJSON(t, parseRequest(t, `{"contents":`+c.contents+`}`).Input, c.want)
		})
	}
}

func TestParseRequest_Config(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			"options keyed by the body model",
			`{"model":"g","generationConfig":{"temperature":0.7,"topP":0.9,"frequencyPenalty":0.1,"presencePenalty":0.2,"thinkingConfig":{"thinkingBudget":512}}}`,
			`{"model":"g","input":null,"model_config":{"g":{"sampling":{"temperature":0.7,"top_p":0.9,"frequency_penalty":0.1,"presence_penalty":0.2},"reasoning":{"budget_tokens":512}}}}`,
		},
		{
			"JSON mime type without a schema is json_object",
			`{"generationConfig":{"responseMimeType":"application/json"}}`,
			`{"model":null,"input":null,"model_config":{"*":{"output":{"format":{"type":"json_object"}}}}}`,
		},
		{
			"text mime type and zero budget set no options",
			`{"generationConfig":{"responseMimeType":"text/plain","thinkingConfig":{"thinkingBudget":0}}}`,
			`{"model":null,"input":null}`,
		},
		{
			"system parts join with newlines",
			`{"systemInstruction":{"parts":[{"text":"a"},{"text":""},{"text":"b"}]}}`,
			`{"model":null,"instructions":"a\nb","input":null}`,
		},
		{
			"declaration without parameters, unrecognised mode reads as auto",
			`{"tools":[{"functionDeclarations":[{"name":"f"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}}`,
			`{"model":null,"input":null,"tools":{"definitions":[{"type":"function","name":"f","parameters":{}}],"choice":"auto"}}`,
		},
		{
			"ANY with several allowed names reads as an allowed required choice",
			`{"tools":[{"functionDeclarations":[{"name":"f"},{"name":"g"},{"name":"h"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["f","g"]}}}`,
			`{"model":null,"input":null,"tools":{"definitions":[{"type":"function","name":"f","parameters":{}},{"type":"function","name":"g","parameters":{}},{"type":"function","name":"h","parameters":{}}],
			  "choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"f"},{"type":"function","name":"g"}]}}}`,
		},
		{
			"VALIDATED with allowed names reads as an allowed auto choice",
			`{"tools":[{"functionDeclarations":[{"name":"f"},{"name":"g"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED","allowedFunctionNames":["g"]}}}`,
			`{"model":null,"input":null,"tools":{"definitions":[{"type":"function","name":"f","parameters":{}},{"type":"function","name":"g","parameters":{}}],
			  "choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"g"}]}}}`,
		},
		{
			"proto field names in config and tools",
			`{"generation_config":{"top_p":0.9,"response_mime_type":"application/json","thinking_config":{"thinking_budget":512}},
			  "tools":[{"function_declarations":[{"name":"f"}]}],"tool_config":{"function_calling_config":{"mode":"ANY","allowed_function_names":["f"]}}}`,
			`{"model":null,"input":null,"model_config":{"*":{"sampling":{"top_p":0.9},"reasoning":{"budget_tokens":512},"output":{"format":{"type":"json_object"}}}},
			  "tools":{"definitions":[{"type":"function","name":"f","parameters":{}}],"choice":{"type":"function","name":"f"}}}`,
		},
		{
			"null schema is json_object, negative budget sets no reasoning",
			`{"generationConfig":{"responseMimeType":"application/json","responseSchema":null,"thinkingConfig":{"thinkingBudget":-1}}}`,
			`{"model":null,"input":null,"model_config":{"*":{"output":{"format":{"type":"json_object"}}}}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertJSON(t, parseRequest(t, c.body), c.want)
		})
	}
}

func TestParseRequest_SingleSamplingField(t *testing.T) {
	for wire, canonical := range map[string]string{
		`"temperature":0.5`: `"temperature":0.5`, `"topP":0.5`: `"top_p":0.5`, `"topK":5`: `"top_k":5`,
		`"maxOutputTokens":5`: `"max_tokens":5`, `"stopSequences":["x"]`: `"stop":["x"]`, `"seed":5`: `"seed":5`,
		`"frequencyPenalty":0.5`: `"frequency_penalty":0.5`, `"presencePenalty":0.5`: `"presence_penalty":0.5`,
	} {
		t.Run(wire, func(t *testing.T) {
			assertJSON(t, parseRequest(t, `{"generationConfig":{`+wire+`}}`), `{"model":null,"input":null,"model_config":{"*":{"sampling":{`+canonical+`}}}}`)
		})
	}
}

func TestParseRequest_MalformedMemberReturnsError(t *testing.T) {
	for _, body := range []string{`{"generationConfig":[]}`, `{"tools":["x"]}`, `{"contents":["x"]}`} {
		if req, err := tr.ParseRequest([]byte(body)); err == nil {
			t.Errorf("%s parsed to %+v, want an error", body, req)
		}
	}
}

func TestParseRequest_FunctionResponseOutputUnwrap(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     string
	}{
		{"text", `{"output":"sunny"}`, "sunny"},
		{"number", `{"output":42}`, "42"},
		{"array", `{"output":[1,2]}`, "[1,2]"},
		// Each of these would re-serialize differently if unwrapped, so the object stays whole.
		{"object value", `{"output":{"a":1}}`, `{"output":{"a":1}}`},
		{"JSON-looking text", `{"output":"42"}`, `{"output":"42"}`},
		{"extra key", `{"output":"x","error":"y"}`, `{"output":"x","error":"y"}`},
		{"no wrapper", `{"temp_c":18}`, `{"temp_c":18}`},
	}
	output := func(t *testing.T, req *v1.Request) string {
		t.Helper()
		if len(req.Input) != 1 {
			t.Fatalf("input %d items, want 1", len(req.Input))
		}
		fco, ok := req.Input[0].(*v1.FunctionCallOutput)
		if !ok {
			t.Fatalf("input[0] is %T", req.Input[0])
		}
		return fco.Output
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := parseRequest(t, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":`+c.response+`}}]}]}`)
			if got := output(t, req); got != c.want {
				t.Fatalf("output: got %s, want %s", got, c.want)
			}
			wire, err := tr.SerializeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			if got := output(t, parseRequest(t, string(wire))); got != c.want {
				t.Errorf("round trip output: got %s, want %s", got, c.want)
			}
		})
	}
}

func TestParseRequest_ModelTurnKeepsPartOrder(t *testing.T) {
	req := parseRequest(t, `{"contents":[
		{"role":"model","parts":[{"text":"Checking."},{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}}]}]}`)
	assertJSON(t, req.Input, `[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]},
		{"type":"function_call","call_id":"f","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"f","output":"{\"ok\":true}"}]`)
}

func TestParseRequest_RolelessContentKeepsMedia(t *testing.T) {
	req := parseRequest(t, `{"contents":[{"parts":[{"text":"Caption this."},{"inlineData":{"mimeType":"image/png","data":"iVBO"}}]}]}`)
	assertJSON(t, req.Input, `[{"type":"message","role":"user","content":[{"type":"input_text","text":"Caption this."},{"type":"input_image","image_url":"data:image/png;base64,iVBO"}]}]`)
}

func TestParseRequest_SnakeCaseFieldNames(t *testing.T) {
	req := parseRequest(t, `{"system_instruction":{"parts":[{"text":"Be terse."}]},"generation_config":{"max_output_tokens":64},
		"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"image/png","data":"iVBO"}}]}]}`)
	assertJSON(t, req, `{"model":null,"instructions":"Be terse.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBO"}]}],
		"model_config":{"*":{"sampling":{"max_tokens":64}}}}`)
}

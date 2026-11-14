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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertJSON(t, parseRequest(t, c.body), c.want)
		})
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
	t.Skip("bug: proto field names (system_instruction, generation_config, inline_data, as in Google's REST examples) are ignored without an error")
	req := parseRequest(t, `{"system_instruction":{"parts":[{"text":"Be terse."}]},"generation_config":{"max_output_tokens":64},
		"contents":[{"role":"user","parts":[{"inline_data":{"mime_type":"image/png","data":"iVBO"}}]}]}`)
	assertJSON(t, req, `{"model":null,"instructions":"Be terse.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBO"}]}],
		"model_config":{"*":{"sampling":{"max_tokens":64}}}}`)
}

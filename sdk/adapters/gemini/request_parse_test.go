package gemini

import (
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

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
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":` + c.response + `}}]}]}`
			req, err := tr.ParseRequest([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			fco := req.Input[0].(*v1.FunctionCallOutput)
			if fco.Output != c.want {
				t.Fatalf("output: got %s, want %s", fco.Output, c.want)
			}
			wire, err := tr.SerializeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			again, err := tr.ParseRequest(wire)
			if err != nil {
				t.Fatal(err)
			}
			if got := again.Input[0].(*v1.FunctionCallOutput).Output; got != c.want {
				t.Errorf("round trip output: got %s, want %s", got, c.want)
			}
		})
	}
}

func TestParseRequest_ResponseMimeJSONWithoutSchema(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json"}}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["*"]
	if opts == nil || opts.Output == nil || opts.Output.Format == nil {
		t.Fatalf("no output format parsed: %+v", opts)
	}
	if f := opts.Output.Format; f.Type != "json_object" || len(f.Schema) != 0 {
		t.Errorf("format: got %+v, want json_object", f)
	}
}

func TestParseRequest_FileDataStaysFilePart(t *testing.T) {
	// ImagePart has no media type for a URL, so an image fileData stays a FilePart to keep its mimeType.
	body := `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"https://example.com/files/x"}}]}]}`
	req, err := tr.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := req.Input[0].(*v1.Message).Content[0].(*v1.FilePart)
	if !ok {
		t.Fatalf("part type: %T", req.Input[0].(*v1.Message).Content[0])
	}
	if fp.FileURL != "https://example.com/files/x" || fp.MediaType != "image/png" {
		t.Errorf("file part: %+v", fp)
	}
}

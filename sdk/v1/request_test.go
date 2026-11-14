package v1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRequest_MarshalJSON(t *testing.T) {
	temp := 0.7
	req := &Request{
		Model:        ModelRefs{"m"},
		Instructions: "be concise",
		Input:        []Item{&Message{Role: RoleUser, Content: []Part{&TextPart{Text: "hi"}}}},
		ModelConfig:  map[string]*ModelOpts{"m": {Sampling: &SamplingParams{Temperature: &temp}}},
		Tools:        &ToolsConfig{Definitions: Tools{&FunctionTool{Name: "search", Parameters: json.RawMessage(`{}`)}}, Choice: &ToolChoice{Mode: "auto"}},
		CacheConfig:  &CacheConfig{Tools: true},
		OutputMode:   OutputModeSync,
		User:         "u1",
		Metadata:     map[string]string{"k": "v"},
		Extensions:   map[string]json.RawMessage{"safety": json.RawMessage(`{"level":2}`)},
	}
	wire := `{"model":"m","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"model_config":{"m":{"sampling":{"temperature":0.7}}},"tools":{"definitions":[{"type":"function","name":"search","parameters":{}}],"choice":"auto"},` +
		`"cache_config":{"tools":true},"output_mode":"sync","user":"u1","metadata":{"k":"v"},"extensions":{"safety":{"level":2}}}`
	wantJSON(t, req, wire)

	back, err := Parse([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(req, back); diff != "" {
		t.Errorf("Parse(Marshal(req)) (-want +got):\n%s", diff)
	}

	// Input is always on the wire, so a request without it shows as null instead of a missing member.
	wantJSON(t, &Request{Model: ModelRefs{"m"}}, `{"model":"m","input":null}`)
}

func TestModelRefs_JSON(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want ModelRefs
		// rewire is what the refs marshal back to; empty means wire itself.
		rewire string
	}{
		{name: "string", wire: `"x"`, want: ModelRefs{"x"}},
		{name: "one-element array collapses to a string", wire: `["x"]`, want: ModelRefs{"x"}, rewire: `"x"`},
		{name: "array", wire: `["x","y"]`, want: ModelRefs{"x", "y"}},
		{name: "empty array", wire: `[]`, want: ModelRefs{}},
		{name: "null", wire: `null`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ModelRefs{"stale"}
			if err := json.Unmarshal([]byte(tc.wire), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("refs (-want +got):\n%s", diff)
			}
			rewire := tc.rewire
			if rewire == "" {
				rewire = tc.wire
			}
			wantJSON(t, got, rewire)
		})
	}

	var m ModelRefs
	wantErr(t, json.Unmarshal([]byte(`{"a":1}`), &m), `model: expected string or array, got "{\"a\":1}"`)
	wantErr(t, json.Unmarshal([]byte(`[1]`), &m), "model: json: cannot unmarshal number…")
	wantErr(t, m.UnmarshalJSON([]byte(`"\x"`)), "model: invalid escape sequence…")
	// The echoed value is cut at 32 bytes.
	wantErr(t, m.UnmarshalJSON([]byte(`1234567890123456789012345678901234567890`)), `model: expected string or array, got "12345678901234567890123456789012"`)
	if err := m.UnmarshalJSON(nil); err != nil || m != nil {
		t.Errorf("empty input: refs %v, err %v; want nil, nil", m, err)
	}
}

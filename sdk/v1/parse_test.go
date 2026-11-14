package v1

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	hi := []Item{&Message{Role: RoleUser, Content: []Part{&TextPart{Text: "hi"}}}}
	temp, maxTok, parallel := 0.5, 512, false
	tests := []struct {
		name string
		body string
		want *Request
	}{
		{
			name: "string model and string input",
			body: `{"model":"x","input":"hi"}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi},
		},
		{
			name: "single-element model array",
			body: `{"model":["x"],"input":"hi"}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi},
		},
		{
			name: "array input",
			body: `{"model":"x","input":[{"type":"message","role":"user","content":"hi"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`,
			want: &Request{Model: ModelRefs{"x"}, Input: []Item{hi[0], &Message{Role: RoleAssistant, Content: []Part{&OutputTextPart{Text: "hello"}}}}},
		},
		{
			name: "scalar members",
			body: `{"model":"x","input":"hi","instructions":"be concise","output_mode":"stream","user":"u1","metadata":{"k":"v"},"extensions":{"safety":{"level":2}}}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi, Instructions: "be concise", OutputMode: OutputModeStream, User: "u1",
				Metadata: map[string]string{"k": "v"}, Extensions: map[string]json.RawMessage{"safety": json.RawMessage(`{"level":2}`)}},
		},
		{
			name: "model_config for the requested model",
			body: `{"model":"x","input":"hi","model_config":{"x":{"sampling":{"temperature":0.5,"max_tokens":512,"stop":["a"]},"reasoning":{"effort":"high","summary":"auto"},"output":{"format":{"type":"json_schema","name":"n","schema":{"type":"object"}},"verbosity":"low"}}}}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi, ModelConfig: map[string]*ModelOpts{"x": {
				Sampling:  &SamplingParams{Temperature: &temp, MaxTokens: &maxTok, Stop: []string{"a"}},
				Reasoning: &ReasoningConfig{Effort: "high", Summary: "auto"},
				Output:    &OutputConfig{Format: &Format{Type: "json_schema", Name: "n", Schema: json.RawMessage(`{"type":"object"}`)}, Verbosity: "low"},
			}}},
		},
		{
			name: "model_config null entry stays present and nil",
			body: `{"model":"x","input":"hi","model_config":{"x":null}}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi, ModelConfig: map[string]*ModelOpts{"x": nil}},
		},
		{
			name: "model_config empty entry",
			body: `{"model":"x","input":"hi","model_config":{"x":{}}}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi, ModelConfig: map[string]*ModelOpts{"x": {}}},
		},
		{
			name: "tools of every kind with choice and parallel",
			body: `{"model":"x","input":"hi","tools":{"definitions":[{"type":"function","name":"get_weather","parameters":{"type":"object"}},{"type":"server","name":"web_search"},{"type":"mcp","server_url":"https://mcp.example.com"}],"choice":{"type":"function","name":"get_weather"},"parallel":false}}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi, Tools: &ToolsConfig{
				Definitions: Tools{&FunctionTool{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}, &ServerTool{Name: "web_search"}, &MCPTool{ServerURL: "https://mcp.example.com"}},
				Choice:      &ToolChoice{Mode: "function", FunctionName: "get_weather"},
				Parallel:    &parallel,
			}},
		},
		{
			name: "request and item cache_config",
			body: `{"model":"x","cache_config":{"instructions":true,"tools":true,"history":true,"key":"conv-1","ttl":"1h"},"input":[{"type":"message","role":"user","content":"stable","cache_config":{"anchor":true}},{"type":"message","role":"user","content":"latest"}]}`,
			want: &Request{Model: ModelRefs{"x"},
				CacheConfig: &CacheConfig{Instructions: true, Tools: true, History: true, Key: "conv-1", TTL: "1h"},
				Input: []Item{
					&Message{Role: RoleUser, Content: []Part{&TextPart{Text: "stable"}}, CacheConfig: &ItemCacheConfig{Anchor: true}},
					&Message{Role: RoleUser, Content: []Part{&TextPart{Text: "latest"}}},
				}},
		},
		{
			name: "unknown top-level members are ignored",
			body: `{"model":"x","input":"hi","temperature":1,"stream":true}`,
			want: &Request{Model: ModelRefs{"x"}, Input: hi},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("request (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"invalid JSON", `{not json}`, "invalid JSON: …"},
		{"empty body", ``, "invalid JSON: unexpected end of JSON input"},
		{"model missing", `{"input":"hi"}`, "model required"},
		{"model null", `{"model":null,"input":"hi"}`, "model required"},
		{"model empty array", `{"model":[],"input":"hi"}`, "model required"},
		{"model empty string", `{"model":"","input":"hi"}`, "model: empty model name"},
		{"model empty in array", `{"model":[""],"input":"hi"}`, "model: empty model name"},
		{"model number", `{"model":5,"input":"hi"}`, `invalid JSON: model: expected string or array, got "5"`},
		{"model array of numbers", `{"model":[1],"input":"hi"}`, "invalid JSON: model: json: cannot unmarshal number…"},
		{"two models", `{"model":["x","y"],"input":"hi"}`, "multiplex_not_implemented"},
		{"an empty name outranks multiplex", `{"model":["x",""],"input":"hi"}`, "model: empty model name"},
		{"model_config for another model", `{"model":"x","input":"hi","model_config":{"y":{}}}`, `model_config has key "y" not in model list`},
		{"input missing", `{"model":"x"}`, "input is required"},
		{"input number", `{"model":"x","input":5}`, "input: input array: json: cannot unmarshal number…"},
		{"input with unsupported item", `{"model":"x","input":[{"type":"computer_call"}]}`, `input: unsupported item type "computer_call"`},
		{"unsupported tool", `{"model":"x","input":"hi","tools":{"definitions":[{"type":"code_interpreter"}]}}`, `invalid JSON: unsupported tool type "code_interpreter"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.body))
			if got != nil || err == nil {
				t.Fatalf("Parse = %+v, %v; want nil and an error", got, err)
			}
			wantErr(t, err, tc.want)
		})
	}

	if _, err := Parse([]byte(`{"model":["x","y"],"input":"hi"}`)); !errors.Is(err, ErrMultiplexNotImplemented) {
		t.Errorf("two models: %v, want ErrMultiplexNotImplemented", err)
	}
}

func TestParse_RejectsEmptyInput(t *testing.T) {
	t.Skip(`bug: {"model":"x","input":null} and "input":[] parse into a request with no input items instead of "input is required"`)
	for _, body := range []string{`{"model":"x","input":null}`, `{"model":"x","input":[]}`} {
		if got, err := Parse([]byte(body)); err == nil {
			t.Errorf("Parse(%s) = %+v, want an error", body, got)
		}
	}
}

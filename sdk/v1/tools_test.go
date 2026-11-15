package v1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestUnmarshalTool(t *testing.T) {
	strict := true
	tests := []struct {
		name string
		wire string
		want Tool
	}{
		{
			"function with every field",
			`{"type":"function","name":"get_weather","description":"get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":true,"provider_data":{"type":"custom"}}`,
			&FunctionTool{Name: "get_weather", Description: "get weather", Strict: &strict,
				Parameters:   json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
				ProviderData: json.RawMessage(`{"type":"custom"}`)},
		},
		{"function schema bytes are kept verbatim", `{"type":"function","name":"f","parameters":{"b":1,"a":[2,1]}}`,
			&FunctionTool{Name: "f", Parameters: json.RawMessage(`{"b":1,"a":[2,1]}`)}},
		{"server", `{"type":"server","name":"web_search"}`, &ServerTool{Name: "web_search"}},
		{"mcp by name", `{"type":"mcp","name":"my-server"}`, &MCPTool{Name: "my-server"}},
		{"mcp by url", `{"type":"mcp","server_url":"https://mcp.example.com","headers":{"Authorization":"Bearer tok"}}`,
			&MCPTool{ServerURL: "https://mcp.example.com", Headers: map[string]string{"Authorization": "Bearer tok"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unmarshalTool([]byte(tc.wire))
			if err != nil {
				t.Fatalf("unmarshalTool: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("tool (-want +got):\n%s", diff)
			}
			if got.ToolType() != tc.want.ToolType() {
				t.Errorf("ToolType = %q, want %q", got.ToolType(), tc.want.ToolType())
			}
			wantJSON(t, got, tc.wire)
		})
	}
}

func TestUnmarshalTool_Errors(t *testing.T) {
	tests := []struct{ name, wire, want string }{
		{"not an object", `1`, "tool: json: cannot unmarshal number…"},
		{"missing type", `{"name":"f"}`, `unsupported tool type ""`},
		{"unknown type", `{"type":"web_search_preview"}`, `unsupported tool type "web_search_preview"`},
		{"function with numeric name", `{"type":"function","name":1}`, "function tool: json: cannot unmarshal number…"},
		{"server with numeric name", `{"type":"server","name":1}`, "server tool: json: cannot unmarshal number…"},
		{"mcp with string headers", `{"type":"mcp","headers":"x"}`, "mcp tool: json: cannot unmarshal string…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unmarshalTool([]byte(tc.wire))
			if err == nil {
				t.Fatalf("unmarshalTool = %#v, want an error", got)
			}
			wantErr(t, err, tc.want)
		})
	}
}

func TestTools_JSON(t *testing.T) {
	wire := `[{"type":"function","name":"f1","parameters":{}},{"type":"server","name":"s"},{"type":"mcp","name":"m"}]`
	var got Tools
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatal(err)
	}
	want := Tools{&FunctionTool{Name: "f1", Parameters: json.RawMessage(`{}`)}, &ServerTool{Name: "s"}, &MCPTool{Name: "m"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tools (-want +got):\n%s", diff)
	}
	wantJSON(t, got, wire)
	wantJSON(t, Tools{}, `[]`)

	prior := Tools{&ServerTool{Name: "kept"}}
	for _, tc := range []struct{ name, wire, want string }{
		{"not an array", `{}`, "tools array: json: cannot unmarshal object…"},
		{"bad element", `[{"type":"server","name":"s"},{"type":"x"}]`, `unsupported tool type "x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := prior
			wantErr(t, json.Unmarshal([]byte(tc.wire), &ts), tc.want)
			if diff := cmp.Diff(prior, ts); diff != "" {
				t.Errorf("a failed decode changed the receiver (-want +got):\n%s", diff)
			}
		})
	}
}

func TestToolChoice_JSON(t *testing.T) {
	tests := []struct {
		wire string
		want ToolChoice
		// rewire is what the choice marshals back to; empty means wire itself.
		rewire string
	}{
		{wire: `"auto"`, want: ToolChoice{Mode: "auto"}},
		{wire: `"required"`, want: ToolChoice{Mode: "required"}},
		{wire: `"none"`, want: ToolChoice{Mode: "none"}},
		{wire: `{"type":"function","name":"get_weather"}`, want: ToolChoice{Mode: "function", FunctionName: "get_weather"}},
		{wire: `"any"`, want: ToolChoice{Mode: "any"}},
		{wire: `{"type":"allowed","name":"x"}`, want: ToolChoice{Mode: "allowed", FunctionName: "x"}, rewire: `"allowed"`},
		{wire: `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}]}`, want: ToolChoice{Mode: "auto", Allowed: []string{"a", "b"}}},
		{wire: `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"a"}]}`, want: ToolChoice{Mode: "required", Allowed: []string{"a"}}},
	}
	for _, tc := range tests {
		t.Run(tc.wire, func(t *testing.T) {
			var got ToolChoice
			if err := json.Unmarshal([]byte(tc.wire), &got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("choice (-want +got):\n%s", diff)
			}
			rewire := tc.rewire
			if rewire == "" {
				rewire = tc.wire
			}
			wantJSON(t, got, rewire)
		})
	}

	var tc ToolChoice
	wantErr(t, json.Unmarshal([]byte(`5`), &tc), "tool_choice object: json: cannot unmarshal number…")
	wantErr(t, json.Unmarshal([]byte(`{"type":1}`), &tc), "tool_choice object: json: cannot unmarshal number…")
	wantErr(t, json.Unmarshal([]byte(`{"type":"allowed_tools","mode":"none","tools":[{"type":"function","name":"a"}]}`), &tc), `tool_choice allowed_tools: mode "none", want auto or required`)
	wantErr(t, json.Unmarshal([]byte(`{"type":"allowed_tools","mode":"auto","tools":[]}`), &tc), "tool_choice allowed_tools: tools is empty")
	wantErr(t, json.Unmarshal([]byte(`{"type":"allowed_tools","mode":"auto","tools":[{"type":"mcp","name":"a"}]}`), &tc), `tool_choice allowed_tools: tools[0]: want a named function tool, got type "mcp"`)

	// Allowed is ignored with a mode that does not choose among tools.
	wantJSON(t, ToolChoice{Mode: "function", FunctionName: "a", Allowed: []string{"a"}}, `{"type":"function","name":"a"}`)
	wantJSON(t, ToolChoice{Mode: "none", Allowed: []string{"a"}}, `"none"`)
}

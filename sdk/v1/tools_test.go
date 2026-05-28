package v1

import (
	"encoding/json"
	"testing"
)

func TestFunctionToolRoundTrip(t *testing.T) {
	wire := `{"type":"function","name":"get_weather","description":"get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}`
	t.Run("unmarshal", func(t *testing.T) {
		tool, err := unmarshalTool([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		ft, ok := tool.(*FunctionTool)
		if !ok {
			t.Fatalf("expected *FunctionTool, got %T", tool)
		}
		if ft.Name != "get_weather" {
			t.Errorf("name: %q", ft.Name)
		}
		b, err := json.Marshal(ft)
		if err != nil {
			t.Fatal(err)
		}
		tool2, err := unmarshalTool(b)
		if err != nil {
			t.Fatal(err)
		}
		ft2 := tool2.(*FunctionTool)
		if ft2.Name != ft.Name {
			t.Errorf("name mismatch: %q vs %q", ft2.Name, ft.Name)
		}
		if string(ft2.Parameters) != string(ft.Parameters) {
			t.Errorf("parameters mismatch")
		}
	})
}

func TestServerToolRoundTrip(t *testing.T) {
	wire := `{"type":"server","name":"web_search"}`
	tool, err := unmarshalTool([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := tool.(*ServerTool)
	if !ok {
		t.Fatalf("expected *ServerTool, got %T", tool)
	}
	if st.Name != "web_search" {
		t.Errorf("name: %q", st.Name)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	tool2, _ := unmarshalTool(b)
	st2 := tool2.(*ServerTool)
	if st2.Name != st.Name {
		t.Errorf("name mismatch after round-trip")
	}
}

func TestMCPToolRoundTrip(t *testing.T) {
	t.Run("by name", func(t *testing.T) {
		wire := `{"type":"mcp","name":"my-server"}`
		tool, err := unmarshalTool([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		mt := tool.(*MCPTool)
		if mt.Name != "my-server" {
			t.Errorf("name: %q", mt.Name)
		}
	})

	t.Run("by url", func(t *testing.T) {
		wire := `{"type":"mcp","server_url":"https://mcp.example.com","headers":{"Authorization":"Bearer tok"}}`
		tool, err := unmarshalTool([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		mt := tool.(*MCPTool)
		if mt.ServerURL != "https://mcp.example.com" {
			t.Errorf("server_url: %q", mt.ServerURL)
		}
		if mt.Headers["Authorization"] != "Bearer tok" {
			t.Errorf("headers: %v", mt.Headers)
		}
		b, err := json.Marshal(mt)
		if err != nil {
			t.Fatal(err)
		}
		tool2, _ := unmarshalTool(b)
		mt2 := tool2.(*MCPTool)
		if mt2.ServerURL != mt.ServerURL {
			t.Errorf("server_url mismatch after round-trip")
		}
	})
}

func TestToolsSliceRoundTrip(t *testing.T) {
	wire := `[{"type":"function","name":"f1","parameters":{}},{"type":"function","name":"f2","parameters":{}}]`
	var tools Tools
	if err := json.Unmarshal([]byte(wire), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	var tools2 Tools
	if err := json.Unmarshal(b, &tools2); err != nil {
		t.Fatal(err)
	}
	if len(tools2) != len(tools) {
		t.Errorf("tools length mismatch after round-trip")
	}
}

func TestToolChoiceRoundTrip(t *testing.T) {
	tests := []struct {
		wire string
		mode string
		fn   string
	}{
		{`"auto"`, "auto", ""},
		{`"required"`, "required", ""},
		{`"none"`, "none", ""},
		{`{"type":"function","name":"get_weather"}`, "function", "get_weather"},
	}
	for _, tc := range tests {
		var choice ToolChoice
		if err := json.Unmarshal([]byte(tc.wire), &choice); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.wire, err)
		}
		if choice.Mode != tc.mode {
			t.Errorf("mode: got %q, want %q", choice.Mode, tc.mode)
		}
		if choice.FunctionName != tc.fn {
			t.Errorf("function_name: got %q, want %q", choice.FunctionName, tc.fn)
		}
		b, err := json.Marshal(choice)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var choice2 ToolChoice
		if err := json.Unmarshal(b, &choice2); err != nil {
			t.Fatalf("re-unmarshal: %v", err)
		}
		if choice2.Mode != choice.Mode {
			t.Errorf("mode mismatch after round-trip")
		}
	}
}

func TestUnsupportedToolTypeError(t *testing.T) {
	_, err := unmarshalTool([]byte(`{"type":"unknown_tool_xyz"}`))
	if err == nil {
		t.Error("expected error for unknown tool type, got nil")
	}
}

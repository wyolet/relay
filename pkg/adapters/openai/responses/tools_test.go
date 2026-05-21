package responses

import (
	"encoding/json"
	"testing"
)

func TestFunctionToolRoundTrip(t *testing.T) {
	wire := `{"type":"function","name":"get_weather","description":"Gets weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}`
	var tools Tools
	err := json.Unmarshal([]byte(`[`+wire+`]`), &tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	ft, ok := tools[0].(*FunctionTool)
	if !ok {
		t.Fatalf("expected *FunctionTool, got %T", tools[0])
	}
	if ft.Name != "get_weather" {
		t.Errorf("name: %q", ft.Name)
	}
	// Marshal back and re-parse.
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	var tools2 Tools
	if err := json.Unmarshal(b, &tools2); err != nil {
		t.Fatal(err)
	}
	ft2 := tools2[0].(*FunctionTool)
	if ft2.Description != ft.Description {
		t.Errorf("description mismatch: %q vs %q", ft2.Description, ft.Description)
	}
}

func TestUnsupportedToolTypeReturnsError(t *testing.T) {
	unsupported := []string{
		"web_search_preview",
		"web_search",
		"file_search",
		"code_interpreter",
		"computer_use_preview",
		"image_generation",
		"mcp",
		"custom",
	}
	for _, typ := range unsupported {
		wire := `[{"type":"` + typ + `"}]`
		var tools Tools
		err := json.Unmarshal([]byte(wire), &tools)
		if err == nil {
			t.Errorf("expected error for tool type %q, got nil", typ)
		}
	}
}

func TestToolChoiceStringRoundTrip(t *testing.T) {
	tests := []struct {
		wire string
		mode string
	}{
		{`"auto"`, "auto"},
		{`"required"`, "required"},
		{`"none"`, "none"},
	}
	for _, tc := range tests {
		var choice ToolChoice
		if err := json.Unmarshal([]byte(tc.wire), &choice); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.wire, err)
		}
		if choice.Mode != tc.mode {
			t.Errorf("mode: got %q, want %q", choice.Mode, tc.mode)
		}
		b, err := json.Marshal(choice)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.wire {
			t.Errorf("marshal: got %s, want %s", b, tc.wire)
		}
	}
}

func TestToolChoiceFunctionObjectRoundTrip(t *testing.T) {
	wire := `{"type":"function","name":"foo"}`
	var choice ToolChoice
	if err := json.Unmarshal([]byte(wire), &choice); err != nil {
		t.Fatal(err)
	}
	if choice.Mode != "function" {
		t.Errorf("mode: %q", choice.Mode)
	}
	if choice.FunctionName != "foo" {
		t.Errorf("function name: %q", choice.FunctionName)
	}
	b, err := json.Marshal(choice)
	if err != nil {
		t.Fatal(err)
	}
	var choice2 ToolChoice
	if err := json.Unmarshal(b, &choice2); err != nil {
		t.Fatal(err)
	}
	if choice2.FunctionName != "foo" {
		t.Errorf("function name after round-trip: %q", choice2.FunctionName)
	}
}

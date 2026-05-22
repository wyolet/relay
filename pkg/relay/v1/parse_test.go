package v1

import (
	"testing"
)

func TestParseStringInput(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hello"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("model: %q", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("expected 1 item, got %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*Message)
	if !ok {
		t.Fatalf("expected *Message, got %T", req.Input[0])
	}
	if msg.Role != RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Content))
	}
	tp, ok := msg.Content[0].(*TextPart)
	if !ok {
		t.Fatalf("expected *TextPart, got %T", msg.Content[0])
	}
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
}

func TestParseArrayInput(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":"hi"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(req.Input))
	}
}

func TestParseRequiresModel(t *testing.T) {
	body := `{"input":"hi"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error when model is missing")
	}
}

func TestParseRequiresInput(t *testing.T) {
	body := `{"model":"gpt-4o"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error when input is missing")
	}
}

func TestParseWithTools(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "what is the weather?",
		"tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
		"tool_choice": "auto"
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
	if req.Tools[0].ToolType() != ToolTypeFunction {
		t.Errorf("tool type: %v", req.Tools[0].ToolType())
	}
	if req.ToolChoice == nil {
		t.Fatal("expected tool_choice to be parsed")
	}
	if req.ToolChoice.Mode != "auto" {
		t.Errorf("tool_choice mode: %q", req.ToolChoice.Mode)
	}
}

func TestParseWithExtensions(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","extensions":{"cache_control":{"type":"ephemeral"}}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(req.Extensions))
	}
	if _, ok := req.Extensions["cache_control"]; !ok {
		t.Error("expected cache_control extension")
	}
}

func TestParseWithServerAndMCPTools(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "search for cats",
		"tools": [
			{"type":"server","name":"web_search"},
			{"type":"mcp","server_url":"https://mcp.example.com"}
		]
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(req.Tools))
	}
	if req.Tools[0].ToolType() != ToolTypeServer {
		t.Errorf("tool[0] type: %v", req.Tools[0].ToolType())
	}
	if req.Tools[1].ToolType() != ToolTypeMCP {
		t.Errorf("tool[1] type: %v", req.Tools[1].ToolType())
	}
}

func TestParseInvalidJSON(t *testing.T) {
	_, err := Parse([]byte(`{not json}`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseReasoningConfig(t *testing.T) {
	body := `{"model":"o3","input":"think hard","reasoning":{"effort":"high"}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Reasoning == nil {
		t.Fatal("expected reasoning config")
	}
	if req.Reasoning.Effort != "high" {
		t.Errorf("effort: %q", req.Reasoning.Effort)
	}
}

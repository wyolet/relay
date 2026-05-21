package responses

import (
	"testing"
)

func TestParseMinimal(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hello"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("model: %q", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("input length: %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*Message)
	if !ok {
		t.Fatalf("input[0]: %T", req.Input[0])
	}
	if msg.Role != RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content length: %d", len(msg.Content))
	}
	tp, ok := msg.Content[0].(*TextPart)
	if !ok {
		t.Fatalf("content[0]: %T", msg.Content[0])
	}
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
}

func TestParseInputArray(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"what is 2+2?"}]},
			{"type":"function_call","call_id":"c1","name":"calc","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"4"}
		]
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 3 {
		t.Fatalf("input length: %d", len(req.Input))
	}
}

func TestParseWithTools(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "hello",
		"tools": [{"type":"function","name":"do_thing","parameters":{}}],
		"tool_choice": "auto"
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools length: %d", len(req.Tools))
	}
	if req.ToolChoice == nil {
		t.Fatal("tool_choice is nil")
	}
	if req.ToolChoice.Mode != "auto" {
		t.Errorf("tool_choice mode: %q", req.ToolChoice.Mode)
	}
}

func TestParseWithFunctionToolChoice(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "hi",
		"tools": [{"type":"function","name":"search","parameters":{}}],
		"tool_choice": {"type":"function","name":"search"}
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice == nil {
		t.Fatal("tool_choice nil")
	}
	if req.ToolChoice.Mode != "function" || req.ToolChoice.FunctionName != "search" {
		t.Errorf("tool_choice: %+v", req.ToolChoice)
	}
}

func TestParseUnsupportedItemType(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"file_search_call","id":"fsc_1"}]}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("expected error for unsupported item type, got nil")
	}
}

func TestParseUnsupportedToolType(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","tools":[{"type":"file_search"}]}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("expected error for unsupported tool type, got nil")
	}
}

func TestParseMissingModel(t *testing.T) {
	body := `{"input":"hello"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestParseMissingInput(t *testing.T) {
	body := `{"model":"gpt-4o"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("expected error for missing input")
	}
}

func TestParseMetadataValidation(t *testing.T) {
	t.Run("valid metadata passes", func(t *testing.T) {
		body := `{"model":"gpt-4o","input":"hi","metadata":{"user_id":"u123","session":"s1"}}`
		req, err := Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if req.Metadata["user_id"] != "u123" {
			t.Errorf("metadata: %v", req.Metadata)
		}
	})

	t.Run("too many entries rejected", func(t *testing.T) {
		meta := `"metadata":{`
		for i := 0; i < 17; i++ {
			if i > 0 {
				meta += ","
			}
			meta += `"key` + string(rune('a'+i)) + `":"v"`
		}
		meta += `}`
		body := `{"model":"gpt-4o","input":"hi",` + meta + `}`
		_, err := Parse([]byte(body))
		if err == nil {
			t.Fatal("expected error for >16 metadata entries")
		}
	})
}

func TestParsePreviousResponseIDPreserved(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","previous_response_id":"resp_old_123"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.PreviousResponseID != "resp_old_123" {
		t.Errorf("previous_response_id: %q", req.PreviousResponseID)
	}
}

func TestParseForwardCompatFields(t *testing.T) {
	store := true
	bg := false
	body := `{"model":"gpt-4o","input":"hi","store":true,"background":false,"truncation":"auto","service_tier":"default"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Store == nil || *req.Store != store {
		t.Errorf("store: %v", req.Store)
	}
	if req.Background == nil || *req.Background != bg {
		t.Errorf("background: %v", req.Background)
	}
	if req.Truncation != "auto" {
		t.Errorf("truncation: %q", req.Truncation)
	}
}

func TestParseOptionalNumerics(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","temperature":0.7,"top_p":0.9,"max_output_tokens":1024}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Errorf("temperature: %v", req.Temperature)
	}
	if req.MaxOutputTokens == nil || *req.MaxOutputTokens != 1024 {
		t.Errorf("max_output_tokens: %v", req.MaxOutputTokens)
	}
}

func TestParseStream(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","stream":true}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Stream == nil || !*req.Stream {
		t.Error("stream should be true")
	}
}

func TestRoutingHints(t *testing.T) {
	body := `{"model":"gpt-4o-mini","input":"test","stream":true,"user":"u1","previous_response_id":"resp_123"}`
	h, err := RoutingHints([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if h.Model != "gpt-4o-mini" {
		t.Errorf("model: %q", h.Model)
	}
	if !h.Stream {
		t.Error("stream should be true")
	}
	if h.User != "u1" {
		t.Errorf("user: %q", h.User)
	}
	if h.PreviousResponseID != "resp_123" {
		t.Errorf("previous_response_id: %q", h.PreviousResponseID)
	}
}

func TestRoutingHintsMissingModel(t *testing.T) {
	body := `{"input":"hi"}`
	_, err := RoutingHints([]byte(body))
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

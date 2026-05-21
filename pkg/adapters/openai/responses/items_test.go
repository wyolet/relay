package responses

import (
	"encoding/json"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{
			name: "string content normalizes to TextPart",
			wire: `{"type":"message","role":"user","content":"hello world"}`,
		},
		{
			name: "array content with input_text",
			wire: `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		},
		{
			name: "assistant message with status",
			wire: `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}`,
		},
		{
			name: "system message",
			wire: `{"type":"message","role":"system","content":[{"type":"input_text","text":"you are helpful"}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item, err := unmarshalItem([]byte(tc.wire))
			if err != nil {
				t.Fatalf("unmarshalItem: %v", err)
			}
			msg, ok := item.(*Message)
			if !ok {
				t.Fatalf("expected *Message, got %T", item)
			}
			if len(msg.Content) == 0 {
				t.Fatal("expected non-empty Content")
			}
			// Marshal back.
			got, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Re-parse to verify round-trip fidelity (field values preserved).
			item2, err := unmarshalItem(got)
			if err != nil {
				t.Fatalf("re-unmarshal: %v", err)
			}
			msg2 := item2.(*Message)
			if msg2.Role != msg.Role {
				t.Errorf("role: got %q, want %q", msg2.Role, msg.Role)
			}
			if len(msg2.Content) != len(msg.Content) {
				t.Errorf("content length: got %d, want %d", len(msg2.Content), len(msg.Content))
			}
		})
	}
}

func TestMessageStringContentNormalization(t *testing.T) {
	wire := `{"type":"message","role":"user","content":"hello"}`
	item, err := unmarshalItem([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	msg := item.(*Message)
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Content))
	}
	tp, ok := msg.Content[0].(*TextPart)
	if !ok {
		t.Fatalf("expected *TextPart, got %T", msg.Content[0])
	}
	if tp.Text != "hello" {
		t.Errorf("text: got %q, want %q", tp.Text, "hello")
	}
}

func TestFunctionCallRoundTrip(t *testing.T) {
	wire := `{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"NYC\"}","status":"completed"}`
	item, err := unmarshalItem([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := item.(*FunctionCall)
	if !ok {
		t.Fatalf("expected *FunctionCall, got %T", item)
	}
	if fc.Name != "get_weather" {
		t.Errorf("name: %q", fc.Name)
	}
	if fc.CallID != "call_abc" {
		t.Errorf("call_id: %q", fc.CallID)
	}
	// Marshal and re-parse.
	b, _ := json.Marshal(fc)
	item2, err := unmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	fc2 := item2.(*FunctionCall)
	if fc2.Arguments != fc.Arguments {
		t.Errorf("arguments mismatch: %q vs %q", fc2.Arguments, fc.Arguments)
	}
}

func TestFunctionCallOutputRoundTrip(t *testing.T) {
	t.Run("string output", func(t *testing.T) {
		wire := `{"type":"function_call_output","call_id":"call_abc","output":"sunny"}`
		item, err := unmarshalItem([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		fco := item.(*FunctionCallOutput)
		if fco.Output != "sunny" {
			t.Errorf("output: %q", fco.Output)
		}
		b, _ := json.Marshal(fco)
		item2, _ := unmarshalItem(b)
		fco2 := item2.(*FunctionCallOutput)
		if fco2.Output != fco.Output {
			t.Errorf("output mismatch after round-trip")
		}
	})

	t.Run("content array output", func(t *testing.T) {
		wire := `{"type":"function_call_output","call_id":"call_abc","content":[{"type":"input_text","text":"done"}]}`
		item, err := unmarshalItem([]byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		fco := item.(*FunctionCallOutput)
		if len(fco.Content) != 1 {
			t.Fatalf("expected 1 content part, got %d", len(fco.Content))
		}
	})
}

func TestReasoningRoundTrip(t *testing.T) {
	wire := `{"type":"reasoning","id":"rs_1","summary":[{"text":"step 1"},{"text":"step 2"}],"status":"completed"}`
	item, err := unmarshalItem([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := item.(*Reasoning)
	if !ok {
		t.Fatalf("expected *Reasoning, got %T", item)
	}
	if len(r.Summary) != 2 {
		t.Errorf("summary length: %d", len(r.Summary))
	}
	b, _ := json.Marshal(r)
	item2, err := unmarshalItem(b)
	if err != nil {
		t.Fatal(err)
	}
	r2 := item2.(*Reasoning)
	if r2.ID != r.ID {
		t.Errorf("id mismatch: %q vs %q", r2.ID, r.ID)
	}
	if len(r2.Summary) != len(r.Summary) {
		t.Errorf("summary length mismatch: %d vs %d", len(r2.Summary), len(r.Summary))
	}
}

func TestMixedItemsArrayRoundTrip(t *testing.T) {
	wire := `[
		{"type":"message","role":"user","content":"what is the weather?"},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"}
	]`
	items, err := unmarshalItems([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].ItemType() != ItemTypeMessage {
		t.Errorf("item[0]: %v", items[0].ItemType())
	}
	if items[1].ItemType() != ItemTypeFunctionCall {
		t.Errorf("item[1]: %v", items[1].ItemType())
	}
	if items[2].ItemType() != ItemTypeFunctionCallOutput {
		t.Errorf("item[2]: %v", items[2].ItemType())
	}
}

func TestUnsupportedItemTypeReturnsError(t *testing.T) {
	unsupported := []string{
		"file_search_call",
		"web_search_call",
		"computer_call",
		"computer_call_output",
		"code_interpreter_call",
		"image_generation_call",
		"mcp_call",
		"mcp_list_tools",
		"compaction",
	}
	for _, typ := range unsupported {
		wire := `{"type":"` + typ + `"}`
		_, err := unmarshalItem([]byte(wire))
		if err == nil {
			t.Errorf("expected error for type %q, got nil", typ)
		}
	}
}

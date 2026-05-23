package v1

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

func TestResponseMarshalRoundTrip(t *testing.T) {
	resp := &Response{
		ID:           "resp_abc",
		Object:       "response",
		CreatedAt:    1716000000,
		Model:        "gpt-4o",
		Status:       StatusCompleted,
		FinishReason: FinishReasonStop,
		Output: []Item{
			&Message{
				ID:     "msg_1",
				Status: StatusCompleted,
				Role:   RoleAssistant,
				Content: []Part{
					&OutputTextPart{Text: "Hello, world!"},
				},
			},
		},
		Usage: usage.Tokens{
			"input":  10,
			"output": 5,
		},
	}

	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	resp2, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatal(err)
	}

	if resp2.ID != resp.ID {
		t.Errorf("id: got %q, want %q", resp2.ID, resp.ID)
	}
	if resp2.Object != resp.Object {
		t.Errorf("object: got %q, want %q", resp2.Object, resp.Object)
	}
	if resp2.Model != resp.Model {
		t.Errorf("model: got %q, want %q", resp2.Model, resp.Model)
	}
	if resp2.Status != resp.Status {
		t.Errorf("status: got %q, want %q", resp2.Status, resp.Status)
	}
	if resp2.FinishReason != resp.FinishReason {
		t.Errorf("finish_reason: got %q, want %q", resp2.FinishReason, resp.FinishReason)
	}
	if len(resp2.Output) != 1 {
		t.Fatalf("output length: got %d, want 1", len(resp2.Output))
	}
	msg := resp2.Output[0].(*Message)
	if msg.Role != RoleAssistant {
		t.Errorf("output[0] role: %q", msg.Role)
	}
	if len(resp2.Usage) == 0 {
		t.Fatal("expected usage")
	}
	if got := resp2.Usage.Sum(); got != 15 {
		t.Errorf("sum: %d (input+output should be 15)", got)
	}
}

func TestResponseNoRequestEchoFields(t *testing.T) {
	resp := &Response{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1716000000,
		Model:     "gpt-4o",
		Status:    StatusCompleted,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	// None of the request-echo fields should appear.
	for _, field := range []string{"instructions", "temperature", "top_p", "tools", "tool_choice", "metadata", "parallel_tool_calls"} {
		if containsField(b, field) {
			t.Errorf("response contains request-echo field %q — should not be present", field)
		}
	}
}

func TestResponseWithError(t *testing.T) {
	resp := &Response{
		ID:        "resp_err",
		Object:    "response",
		CreatedAt: 1716000000,
		Model:     "gpt-4o",
		Status:    StatusFailed,
		Error:     &Error{Code: "server_error", Message: "upstream failed"},
	}
	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Error == nil {
		t.Fatal("expected error in response")
	}
	if resp2.Error.Code != "server_error" {
		t.Errorf("error code: %q", resp2.Error.Code)
	}
}

func TestResponseWithIncompleteDetails(t *testing.T) {
	resp := &Response{
		ID:        "resp_inc",
		Object:    "response",
		CreatedAt: 1716000000,
		Model:     "gpt-4o",
		Status:    StatusIncomplete,
		IncompleteDetails: &IncompleteDetails{Reason: "max_output_tokens"},
	}
	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.IncompleteDetails == nil {
		t.Fatal("expected incomplete_details")
	}
	if resp2.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("reason: %q", resp2.IncompleteDetails.Reason)
	}
}

func TestResponseWithExtensions(t *testing.T) {
	resp := &Response{
		ID:        "resp_ext",
		Object:    "response",
		CreatedAt: 1716000000,
		Model:     "gpt-4o",
		Status:    StatusCompleted,
		Extensions: map[string]json.RawMessage{
			"cache_hit": json.RawMessage(`true`),
		},
	}
	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(resp2.Extensions))
	}
}

func TestResponseRefusalIsFinishReason(t *testing.T) {
	resp := &Response{
		ID:           "resp_ref",
		Object:       "response",
		CreatedAt:    1716000000,
		Model:        "gpt-4o",
		Status:       StatusCompleted,
		FinishReason: FinishReasonRefusal,
		Output: []Item{
			&Message{
				Role:    RoleAssistant,
				Content: []Part{&OutputTextPart{Text: "I can't help with that."}},
			},
		},
	}
	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.FinishReason != FinishReasonRefusal {
		t.Errorf("finish_reason: got %q, want %q", resp2.FinishReason, FinishReasonRefusal)
	}
	// The refusal text is in the message content, not a separate item type.
	if len(resp2.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(resp2.Output))
	}
	msg := resp2.Output[0].(*Message)
	op := msg.Content[0].(*OutputTextPart)
	if op.Text != "I can't help with that." {
		t.Errorf("refusal text: %q", op.Text)
	}
}

// containsField is a simple check for whether a JSON field name appears in a
// marshaled byte slice. Not a full parser; sufficient for these tests.
func containsField(b []byte, field string) bool {
	key := `"` + field + `"`
	for i := 0; i+len(key) <= len(b); i++ {
		if string(b[i:i+len(key)]) == key {
			return true
		}
	}
	return false
}

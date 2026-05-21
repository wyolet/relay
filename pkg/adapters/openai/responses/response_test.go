package responses

import (
	"encoding/json"
	"testing"
)

func TestResponseRoundTrip(t *testing.T) {
	resp := &Response{
		ID:        "resp_abc123",
		Object:    "response",
		CreatedAt: 1700000000,
		Model:     "gpt-4o",
		Status:    StatusCompleted,
		Output: []Item{
			&Message{
				ID:     "msg_out_1",
				Status: StatusCompleted,
				Role:   RoleAssistant,
				Content: []Part{
					&OutputTextPart{
						Text: "The answer is 42.",
						Annotations: []Annotation{
							&URLCitationAnnotation{
								StartIndex: 4,
								EndIndex:   10,
								URL:        "https://example.com",
								Title:      "Example",
							},
						},
					},
				},
			},
		},
		Usage: &Usage{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
			InputTokensDetails: &InputDeets{
				CachedTokens: 3,
			},
		},
	}

	b, err := Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	resp2, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatalf("UnmarshalResponse: %v", err)
	}

	if resp2.ID != resp.ID {
		t.Errorf("id: %q vs %q", resp2.ID, resp.ID)
	}
	if resp2.Model != resp.Model {
		t.Errorf("model: %q vs %q", resp2.Model, resp.Model)
	}
	if resp2.Status != resp.Status {
		t.Errorf("status: %q vs %q", resp2.Status, resp.Status)
	}
	if len(resp2.Output) != 1 {
		t.Fatalf("output length: %d", len(resp2.Output))
	}

	msg := resp2.Output[0].(*Message)
	if msg.Role != RoleAssistant {
		t.Errorf("role: %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content length: %d", len(msg.Content))
	}
	otp := msg.Content[0].(*OutputTextPart)
	if otp.Text != "The answer is 42." {
		t.Errorf("text: %q", otp.Text)
	}
	if len(otp.Annotations) != 1 {
		t.Fatalf("annotations length: %d", len(otp.Annotations))
	}
	uc := otp.Annotations[0].(*URLCitationAnnotation)
	if uc.URL != "https://example.com" {
		t.Errorf("url: %q", uc.URL)
	}

	// Usage with input_tokens_details.
	if resp2.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp2.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens: %d", resp2.Usage.TotalTokens)
	}
	if resp2.Usage.InputTokensDetails == nil {
		t.Fatal("input_tokens_details is nil")
	}
	if resp2.Usage.InputTokensDetails.CachedTokens != 3 {
		t.Errorf("cached_tokens: %d", resp2.Usage.InputTokensDetails.CachedTokens)
	}
}

func TestResponseWithOutputTokensDetails(t *testing.T) {
	resp := &Response{
		ID:     "resp_1",
		Object: "response",
		Model:  "o1",
		Status: StatusCompleted,
		Output: []Item{},
		Usage: &Usage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
			OutputTokensDetails: &OutputDeets{
				ReasoningTokens: 20,
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
	if resp2.Usage.OutputTokensDetails == nil {
		t.Fatal("output_tokens_details is nil")
	}
	if resp2.Usage.OutputTokensDetails.ReasoningTokens != 20 {
		t.Errorf("reasoning_tokens: %d", resp2.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

func TestResponseFailedWithError(t *testing.T) {
	resp := &Response{
		ID:     "resp_fail",
		Object: "response",
		Model:  "gpt-4o",
		Status: StatusFailed,
		Output: []Item{},
		Error: &Error{
			Code:    "rate_limit_exceeded",
			Message: "too many requests",
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
	if resp2.Error == nil {
		t.Fatal("error is nil")
	}
	if resp2.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error code: %q", resp2.Error.Code)
	}
}

func TestResponseIncomplete(t *testing.T) {
	resp := &Response{
		ID:           "resp_inc",
		Object:       "response",
		Model:        "gpt-4o",
		Status:       StatusIncomplete,
		FinishReason: FinishReasonLength,
		Output:       []Item{},
		IncompleteDetails: &IncompleteDetails{
			Reason: "max_output_tokens",
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
	if resp2.IncompleteDetails == nil {
		t.Fatal("incomplete_details is nil")
	}
	if resp2.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("reason: %q", resp2.IncompleteDetails.Reason)
	}
	if resp2.FinishReason != FinishReasonLength {
		t.Errorf("finish_reason: %q", resp2.FinishReason)
	}
}

func TestResponseWithReasoningItem(t *testing.T) {
	resp := &Response{
		ID:     "resp_r",
		Object: "response",
		Model:  "o1",
		Status: StatusCompleted,
		Output: []Item{
			&Reasoning{
				ID: "rs_1",
				Summary: []SummaryText{
					{Text: "Let me think..."},
				},
				Status: StatusCompleted,
			},
			&Message{
				Role: RoleAssistant,
				Content: []Part{
					&OutputTextPart{Text: "42"},
				},
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
	if len(resp2.Output) != 2 {
		t.Fatalf("output length: %d", len(resp2.Output))
	}
	if resp2.Output[0].ItemType() != ItemTypeReasoning {
		t.Errorf("output[0] type: %v", resp2.Output[0].ItemType())
	}
}

// TestMarshalIsJSON verifies Marshal returns valid JSON.
func TestMarshalIsJSON(t *testing.T) {
	resp := &Response{
		ID:     "r",
		Object: "response",
		Model:  "gpt-4o",
		Status: StatusCompleted,
		Output: []Item{},
	}
	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
}

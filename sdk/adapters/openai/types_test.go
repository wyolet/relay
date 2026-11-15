package openai

import (
	"encoding/json"
	"testing"
)

func TestFullChatRequest_Validate(t *testing.T) {
	cases := []struct {
		req  FullChatRequest
		want string
	}{
		{FullChatRequest{Messages: []ChatMessage{{Role: "user"}}}, "model is required"},
		{FullChatRequest{Model: "m"}, "messages must contain at least one entry"},
		{FullChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user"}, {}}}, "messages[1].role is required"},
		{FullChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user"}}}, ""},
	}
	for _, tc := range cases {
		got := ""
		if err := tc.req.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("Validate(%+v) = %q, want %q", tc.req, got, tc.want)
		}
	}
}

func TestChatResponseMessage_NullOrAbsentContentStaysNil(t *testing.T) {
	for _, body := range []string{`{"role":"assistant","content":null}`, `{"role":"assistant"}`} {
		var m ChatResponseMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil || m.Content != nil {
			t.Errorf("%s: content = %v (%v), want nil", body, m.Content, err)
		}
	}
}

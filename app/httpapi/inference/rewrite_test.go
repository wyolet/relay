package inference

import (
	"encoding/json"
	"testing"
)

func TestRewriteModelField(t *testing.T) {
	cases := []struct {
		name string
		body string
		to   string
		want string
	}{
		{"simple", `{"model":"gpt-4o","stream":true}`, "gpt-4o-2024-11-20", `"gpt-4o-2024-11-20"`},
		{"unchanged when equal", `{"model":"x"}`, "x", `"x"`},
		{"adds when missing", `{"stream":true}`, "claude-3", `"claude-3"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := rewriteModelField([]byte(tc.body), tc.to)
			var got map[string]json.RawMessage
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output not valid JSON: %v", err)
			}
			if string(got["model"]) != tc.want {
				t.Fatalf("model = %s, want %s", got["model"], tc.want)
			}
		})
	}
}

func TestRewriteModelField_NestedModelKeyUntouched(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"my model is X"}]}`)
	out := rewriteModelField(body, "gpt-4o-2024-11-20")
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid: %v", err)
	}
	if string(got["model"]) != `"gpt-4o-2024-11-20"` {
		t.Fatalf("top-level model not rewritten: %s", got["model"])
	}
	if string(got["messages"]) != `[{"role":"user","content":"my model is X"}]` {
		t.Fatalf("messages content mutated: %s", got["messages"])
	}
}

func TestRewriteModelField_InvalidJSON(t *testing.T) {
	body := []byte(`not json`)
	out := rewriteModelField(body, "x")
	if string(out) != string(body) {
		t.Fatalf("invalid JSON should pass through unchanged")
	}
}

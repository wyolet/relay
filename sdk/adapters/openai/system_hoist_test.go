package openai

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func hoistReq() *v1.Request {
	hoisted := &v1.Message{
		Role:    v1.RoleSystem,
		Hoist:   true,
		Content: []v1.Part{&v1.TextPart{Text: "typed queries only"}},
	}
	return &v1.Request{
		Model:        v1.ModelRefs{"gpt-4o"},
		Instructions: "base",
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "a"}}},
			&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.TextPart{Text: "b"}}},
			hoisted,
			&v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: "positional rule"}}},
		},
	}
}

func TestCCSerialize_HoistMergesLeadingSystem(t *testing.T) {
	body, err := (CCTranslator{}).SerializeRequest(hoistReq())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	roles := make([]string, len(wire.Messages))
	for i, m := range wire.Messages {
		roles[i] = m.Role
	}
	// leading system (instructions + hoisted), then user, assistant, and the
	// non-hoisted system item kept positional (CC-native)
	want := []string{"system", "user", "assistant", "system"}
	if len(roles) != len(want) {
		t.Fatalf("roles: got %v want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles: got %v want %v", roles, want)
		}
	}
	var lead string
	if err := json.Unmarshal(wire.Messages[0].Content, &lead); err != nil {
		t.Fatal(err)
	}
	if lead != "base\ntyped queries only" {
		t.Errorf("leading system: %q", lead)
	}
}

func TestResponsesSerialize_HoistMergesInstructions(t *testing.T) {
	body, err := (ResponsesTranslator{}).SerializeRequest(hoistReq())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Instructions string `json:"instructions"`
		Input        []struct {
			Role string `json:"role"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Instructions != "base\ntyped queries only" {
		t.Errorf("instructions: %q", wire.Instructions)
	}
	for _, it := range wire.Input {
		if it.Role == "system" {
			return // positional one kept — good
		}
	}
	t.Errorf("non-hoisted system item missing from input")
}

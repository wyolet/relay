package openai

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestResponsesSerialize_HoistMergesInstructions(t *testing.T) {
	body, err := (ResponsesTranslator{}).SerializeRequest(&v1.Request{
		Model:        v1.ModelRefs{"m"},
		Instructions: "base",
		Input: []v1.Item{
			ccUser("a"),
			&v1.Message{Role: v1.RoleSystem, Hoist: true, Content: []v1.Part{&v1.TextPart{Text: "typed"}}},
			&v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: "positional"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Instructions != "base\ntyped" || len(wire.Input) != 2 {
		t.Fatalf("instructions %q, %d input items, want the hoisted text joined and two items: %s", wire.Instructions, len(wire.Input), body)
	}
	assertJSON(t, wire.Input[1], `{"type":"message","role":"system","content":[{"type":"input_text","text":"positional"}]}`)
}

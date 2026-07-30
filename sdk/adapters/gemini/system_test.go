package gemini

import (
	"encoding/json"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func sysMsgItem(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

func userMsgItem(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

func asstMsgItem(text string) *v1.Message {
	return &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.TextPart{Text: text}}}
}

func serializeItems(t *testing.T, items ...v1.Item) map[string]any {
	t.Helper()
	body, err := tr.SerializeRequest(&v1.Request{
		Model: v1.ModelRefs{"gemini-2.5-pro"},
		Input: items,
	})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wireContents(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["contents"].([]any)
	if !ok {
		t.Fatalf("contents missing or wrong type: %T", m["contents"])
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func contentText(c map[string]any) string {
	parts := c["parts"].([]any)
	text, _ := parts[0].(map[string]any)["text"].(string)
	return text
}

func TestGeminiSystemLeading_MergesSystemInstruction(t *testing.T) {
	m := serializeItems(t, sysMsgItem("be nice"), userMsgItem("hi"))
	si, ok := m["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", m["systemInstruction"])
	}
	if contentText(si) != "be nice" {
		t.Errorf("systemInstruction text: %v", si)
	}
	cs := wireContents(t, m)
	if len(cs) != 1 || cs[0]["role"] != "user" {
		t.Errorf("contents: %v", cs)
	}
}

func TestGeminiSystemMid_MarkerUserTurnInPlace(t *testing.T) {
	m := serializeItems(t, userMsgItem("a"), asstMsgItem("b"), sysMsgItem("new rule"))
	if _, present := m["systemInstruction"]; present {
		t.Errorf("systemInstruction should be absent, got %v", m["systemInstruction"])
	}
	cs := wireContents(t, m)
	if len(cs) != 3 {
		t.Fatalf("contents len: %d", len(cs))
	}
	if cs[2]["role"] != "user" || contentText(cs[2]) != v1.WrapSystemMarker("new rule") {
		t.Errorf("marker turn: %v", cs[2])
	}
}

func TestGeminiSystemMid_SystemInstructionStable(t *testing.T) {
	serialize := func(items []v1.Item) map[string]any {
		body, err := tr.SerializeRequest(&v1.Request{
			Model:        v1.ModelRefs{"gemini-2.5-pro"},
			Instructions: "stable instructions",
			Input:        items,
		})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		return m
	}
	base := []v1.Item{userMsgItem("a"), asstMsgItem("b")}
	before := serialize(base)
	after := serialize(append(append([]v1.Item{}, base...), sysMsgItem("mid rule")))
	b, _ := json.Marshal(before["systemInstruction"])
	a, _ := json.Marshal(after["systemInstruction"])
	if string(b) != string(a) {
		t.Errorf("systemInstruction changed: %s → %s", b, a)
	}
}

func TestGeminiSystemMid_DeferredPastToolRound(t *testing.T) {
	m := serializeItems(t,
		userMsgItem("run it"),
		&v1.FunctionCall{CallID: "fn", Name: "fn", Arguments: `{}`},
		sysMsgItem("meanwhile"),
		&v1.FunctionCallOutput{CallID: "fn", Output: `{"ok":true}`},
	)
	cs := wireContents(t, m)
	if len(cs) != 4 {
		t.Fatalf("contents len: %d (%v)", len(cs), cs)
	}
	// functionCall (model) directly followed by functionResponse (user),
	// system turn drained after the round
	if cs[1]["role"] != "model" || cs[2]["role"] != "user" {
		t.Errorf("tool round split: %v", cs)
	}
	if contentText(cs[3]) != v1.WrapSystemMarker("meanwhile") {
		t.Errorf("deferred marker turn: %v", cs[3])
	}
}

func TestGeminiSystemHoist_MergesSystemInstruction(t *testing.T) {
	hoisted := sysMsgItem("typed queries only")
	hoisted.Hoist = true
	m := serializeItems(t, userMsgItem("a"), asstMsgItem("b"), hoisted)
	si, ok := m["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", m["systemInstruction"])
	}
	if contentText(si) != "typed queries only" {
		t.Errorf("systemInstruction: %v", si)
	}
	cs := wireContents(t, m)
	if len(cs) != 2 {
		t.Errorf("hoisted item must not stay in contents: %v", cs)
	}
}

func TestGeminiParse_UnwrapsMarkerUserTurn(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": "a"}}},
			{"role": "user", "parts": []map[string]any{{"text": v1.WrapSystemMarker("rule")}}},
		},
	})
	req, err := tr.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 2 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	msg, ok := req.Input[1].(*v1.Message)
	if !ok || msg.Role != v1.RoleSystem {
		t.Fatalf("input[1]: got %T %v, want restored system message", req.Input[1], msg)
	}
	if tp := msg.Content[0].(*v1.TextPart); tp.Text != "rule" {
		t.Errorf("unwrapped text: %q", tp.Text)
	}
}

func TestGeminiParse_MarkerSubstringStaysUser(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": "quote: " + v1.WrapSystemMarker("x")}}},
		},
	})
	req, err := tr.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok || msg.Role != v1.RoleUser {
		t.Fatalf("embedded marker must stay user, got %v", msg)
	}
}

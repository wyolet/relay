package anthropic

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
	body, err := (AnthropicTranslator{}).SerializeRequest(&v1.Request{
		Model: v1.ModelRefs{"claude-opus-5"},
		Input: items,
	})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return decodeMap(t, body)
}

func wireMessages(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing or wrong type: %T", m["messages"])
	}
	msgs := make([]map[string]any, len(raw))
	for i, r := range raw {
		msgs[i] = r.(map[string]any)
	}
	return msgs
}

func assertRoles(t *testing.T, msgs []map[string]any, want ...string) {
	t.Helper()
	if len(msgs) != len(want) {
		t.Fatalf("message count: got %d want %d (%v)", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i]["role"] != w {
			t.Errorf("messages[%d].role: got %v want %s", i, msgs[i]["role"], w)
		}
	}
}

func TestSystemLeading_MergesTopLevelSystem(t *testing.T) {
	m := serializeItems(t, sysMsgItem("be nice"), userMsgItem("hi"))
	if m["system"] != "be nice" {
		t.Errorf("system: got %v", m["system"])
	}
	assertRoles(t, wireMessages(t, m), "user")
}

func TestSystemMid_StaysPositional(t *testing.T) {
	m := serializeItems(t,
		userMsgItem("a"), asstMsgItem("b"), userMsgItem("c"), sysMsgItem("new rule"))
	if _, present := m["system"]; present {
		t.Errorf("top-level system should be absent, got %v", m["system"])
	}
	msgs := wireMessages(t, m)
	assertRoles(t, msgs, "user", "assistant", "user", "system")
	if msgs[3]["content"] != "new rule" {
		t.Errorf("system content: got %v", msgs[3]["content"])
	}
}

func TestSystemMid_CachePrefixStable(t *testing.T) {
	base := []v1.Item{userMsgItem("a"), asstMsgItem("b"), userMsgItem("c")}
	req := func(items []v1.Item) map[string]any {
		body, err := (AnthropicTranslator{}).SerializeRequest(&v1.Request{
			Model:        v1.ModelRefs{"claude-opus-5"},
			Instructions: "stable instructions",
			Input:        items,
		})
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		return decodeMap(t, body)
	}
	before := req(base)
	after := req(append(append([]v1.Item{}, base...), sysMsgItem("mid rule")))

	if before["system"] != after["system"] {
		t.Errorf("system field changed: %v → %v", before["system"], after["system"])
	}
	bm, am := wireMessages(t, before), wireMessages(t, after)
	if len(am) != len(bm)+1 {
		t.Fatalf("expected one appended message, got %d → %d", len(bm), len(am))
	}
	for i := range bm {
		b, _ := json.Marshal(bm[i])
		a, _ := json.Marshal(am[i])
		if string(b) != string(a) {
			t.Errorf("messages[%d] changed: %s → %s", i, b, a)
		}
	}
}

func TestSystemMid_BeforeAssistantStaysSystem(t *testing.T) {
	m := serializeItems(t, userMsgItem("q"), sysMsgItem("rule"), asstMsgItem("a"))
	assertRoles(t, wireMessages(t, m), "user", "system", "assistant")
}

func TestSystemMid_AfterAssistantFallsBackToUserMarker(t *testing.T) {
	m := serializeItems(t,
		userMsgItem("q"), asstMsgItem("a"), sysMsgItem("rule"), asstMsgItem("b"))
	msgs := wireMessages(t, m)
	assertRoles(t, msgs, "user", "assistant", "user", "assistant")
	if msgs[2]["content"] != v1.WrapSystemMarker("rule") {
		t.Errorf("fallback content: got %v", msgs[2]["content"])
	}
}

func TestSystemMid_BetweenUserTurnsFallsBack(t *testing.T) {
	m := serializeItems(t, userMsgItem("a"), sysMsgItem("rule"), userMsgItem("b"))
	msgs := wireMessages(t, m)
	assertRoles(t, msgs, "user", "user", "user")
	if msgs[1]["content"] != v1.WrapSystemMarker("rule") {
		t.Errorf("fallback content: got %v", msgs[1]["content"])
	}
}

func TestSystemMid_ConsecutiveTrailingStay(t *testing.T) {
	m := serializeItems(t, userMsgItem("a"), sysMsgItem("r1"), sysMsgItem("r2"))
	assertRoles(t, wireMessages(t, m), "user", "system", "system")
}

func TestSystemMid_DeferredPastToolResults(t *testing.T) {
	m := serializeItems(t,
		userMsgItem("run it"),
		&v1.FunctionCall{CallID: "c1", Name: "run", Arguments: `{}`},
		sysMsgItem("user sent more input meanwhile"),
		&v1.FunctionCallOutput{CallID: "c1", Output: "done"},
	)
	msgs := wireMessages(t, m)
	assertRoles(t, msgs, "user", "assistant", "user", "system")
	// the tool_result user turn must directly follow its tool_use turn
	blocks := msgs[2]["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("expected tool_result turn at [2], got %v", msgs[2])
	}
}

func TestSystemMid_AnchorKeepsBreakpoint(t *testing.T) {
	sys := sysMsgItem("rule")
	sys.CacheConfig = &v1.ItemCacheConfig{Anchor: true}
	m := serializeItems(t, userMsgItem("a"), sys)
	msgs := wireMessages(t, m)
	assertRoles(t, msgs, "user", "system")
	blocks, ok := msgs[1]["content"].([]any)
	if !ok {
		t.Fatalf("anchored system content should be block form, got %T", msgs[1]["content"])
	}
	if blocks[0].(map[string]any)["cache_control"] == nil {
		t.Errorf("cache_control missing on anchored system block")
	}
}

func TestParseRequest_SystemRoleInMessages(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": "a"},
			{"role": "system", "content": "mid rule"},
			{"role": "assistant", "content": "b"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 3 {
		t.Fatalf("input len: %d", len(req.Input))
	}
	msg, ok := req.Input[1].(*v1.Message)
	if !ok || msg.Role != v1.RoleSystem {
		t.Fatalf("input[1]: got %T role %v, want system message", req.Input[1], msg)
	}
	if tp := msg.Content[0].(*v1.TextPart); tp.Text != "mid rule" {
		t.Errorf("system text: %q", tp.Text)
	}
}

func TestParseRequest_UnwrapsMarkerUserTurn(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": "a"},
			{"role": "user", "content": v1.WrapSystemMarker("rule")},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := req.Input[1].(*v1.Message)
	if !ok || msg.Role != v1.RoleSystem {
		t.Fatalf("input[1]: got %T %v, want restored system message", req.Input[1], msg)
	}
	if tp := msg.Content[0].(*v1.TextPart); tp.Text != "rule" {
		t.Errorf("unwrapped text: %q", tp.Text)
	}
}

func TestParseRequest_MarkerSubstringStaysUser(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": "quoting: " + v1.WrapSystemMarker("nice try") + " end"},
		},
	})
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok || msg.Role != v1.RoleUser {
		t.Fatalf("embedded marker must stay user, got %v", msg)
	}
}

// Round-trip: canonical → wire → canonical preserves system role + position,
// through both the native role:system form and the marker fallback form.
func TestSystemRoundTrip(t *testing.T) {
	items := []v1.Item{
		userMsgItem("a"),
		asstMsgItem("b"),
		sysMsgItem("rule after assistant"), // fallback form (follows assistant)
		userMsgItem("c"),
		sysMsgItem("trailing rule"), // native form (follows user, ends array)
	}
	body, err := (AnthropicTranslator{}).SerializeRequest(&v1.Request{
		Model: v1.ModelRefs{"claude-opus-5"},
		Input: items,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := (AnthropicTranslator{}).ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != len(items) {
		t.Fatalf("round-trip item count: got %d want %d", len(req.Input), len(items))
	}
	wantRoles := []v1.Role{v1.RoleUser, v1.RoleAssistant, v1.RoleSystem, v1.RoleUser, v1.RoleSystem}
	for i, want := range wantRoles {
		msg, ok := req.Input[i].(*v1.Message)
		if !ok || msg.Role != want {
			t.Errorf("input[%d]: got %T role %v, want %v", i, req.Input[i], msg.Role, want)
		}
	}
	if tp := req.Input[2].(*v1.Message).Content[0].(*v1.TextPart); tp.Text != "rule after assistant" {
		t.Errorf("fallback round-trip text: %q", tp.Text)
	}
}

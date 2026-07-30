package v1

import (
	"encoding/json"
	"testing"
)

func TestSystemMarkerRoundTrip(t *testing.T) {
	wrapped := WrapSystemMarker("rule")
	inner, ok := UnwrapSystemMarker(wrapped)
	if !ok || inner != "rule" {
		t.Fatalf("round-trip: got %q ok=%v", inner, ok)
	}
	if _, ok := UnwrapSystemMarker("prefix " + wrapped); ok {
		t.Error("substring must not unwrap")
	}
	if _, ok := UnwrapSystemMarker(wrapped + " suffix"); ok {
		t.Error("substring must not unwrap")
	}
	if _, ok := UnwrapSystemMarker(SystemMarkerOpen + SystemMarkerClose); ok {
		t.Error("empty body must not unwrap")
	}
}

func TestMessageHoistJSONRoundTrip(t *testing.T) {
	m := &Message{Role: RoleSystem, Hoist: true, Content: []Part{&TextPart{Text: "x"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Hoist {
		t.Error("hoist flag lost in JSON round-trip")
	}
}

func TestSplitHoistedSystem(t *testing.T) {
	hoisted := &Message{Role: RoleSystem, Hoist: true, Content: []Part{&TextPart{Text: "h1"}}}
	hoistedDev := &Message{Role: RoleDeveloper, Hoist: true, Content: []Part{&TextPart{Text: "h2"}}}
	positional := &Message{Role: RoleSystem, Content: []Part{&TextPart{Text: "keep"}}}
	user := &Message{Role: RoleUser, Hoist: true, Content: []Part{&TextPart{Text: "u"}}} // ignored on user

	items, text := SplitHoistedSystem([]Item{user, hoisted, positional, hoistedDev})
	if text != "h1\nh2" {
		t.Errorf("hoisted text: %q", text)
	}
	if len(items) != 2 {
		t.Fatalf("items: got %d want 2", len(items))
	}
	if items[0].(*Message).Role != RoleUser || items[1].(*Message).Content[0].(*TextPart).Text != "keep" {
		t.Errorf("wrong items kept: %v", items)
	}

	// no flags → original slice returned untouched
	orig := []Item{user, positional}
	same, text := SplitHoistedSystem(orig)
	if text != "" || len(same) != 2 {
		t.Errorf("no-flag path: %q %d", text, len(same))
	}
}

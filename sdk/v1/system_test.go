package v1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSystemMarker(t *testing.T) {
	if got := WrapSystemMarker("rule"); got != "<system>\nrule\n</system>" {
		t.Errorf("WrapSystemMarker = %q", got)
	}
	tests := []struct {
		name, text, want string
		ok               bool
	}{
		{"wrapped", "<system>\nrule\n</system>", "rule", true},
		{"wrapped multi-line", "<system>\na\nb\n</system>", "a\nb", true},
		{"nested marker unwraps one level", "<system>\n<system>\nx\n</system>\n</system>", "<system>\nx\n</system>", true},
		{"text before", "prefix <system>\nrule\n</system>", "", false},
		{"text after", "<system>\nrule\n</system> suffix", "", false},
		{"empty body", "<system>\n\n</system>", "", false},
		{"open and close sharing their newline", "<system>\n</system>", "", false},
		{"no newline after the open tag", "<system>rule\n</system>", "", false},
		{"plain text", "rule", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := UnwrapSystemMarker(tc.text); got != tc.want || ok != tc.ok {
				t.Errorf("UnwrapSystemMarker = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSplitHoistedSystem(t *testing.T) {
	text := func(role Role, hoist bool, parts ...Part) *Message {
		return &Message{Role: role, Hoist: hoist, Content: parts}
	}
	user := text(RoleUser, true, &TextPart{Text: "u"}) // the flag is ignored on a user message
	positional := text(RoleSystem, false, &TextPart{Text: "keep"})
	call := &FunctionCall{CallID: "c"}

	items := []Item{
		user,
		text(RoleSystem, true, &TextPart{Text: "h1a"}, &ImagePart{ImageURL: "u"}, &OutputTextPart{Text: "h1b"}),
		positional,
		text(RoleDeveloper, true, &TextPart{Text: "h2"}),
		text(RoleSystem, true),
		call,
	}
	rest, hoisted := SplitHoistedSystem(items)
	if hoisted != "h1ah1b\nh2" {
		t.Errorf("hoisted text = %q, want parts concatenated per message and messages joined by a newline", hoisted)
	}
	if diff := cmp.Diff([]Item{user, positional, call}, rest); diff != "" {
		t.Errorf("remaining items (-want +got):\n%s", diff)
	}
	if len(items) != 6 || items[1].(*Message).Role != RoleSystem {
		t.Error("the input slice was modified")
	}

	// Nothing flagged: the very same slice comes back.
	orig := []Item{user, positional}
	same, hoisted := SplitHoistedSystem(orig)
	if hoisted != "" || len(same) != 2 || &same[0] != &orig[0] {
		t.Errorf("no-flag path: text %q, items %v; want the input slice itself", hoisted, same)
	}
	if rest, hoisted := SplitHoistedSystem(nil); rest != nil || hoisted != "" {
		t.Errorf("nil input: %v, %q", rest, hoisted)
	}
}

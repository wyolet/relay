package anthropic

import (
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Mid-conversation system messages: canonical system/developer items that
// appear after conversation content stay positional — they emit as
// role:system wire messages so the cached prefix before them is untouched.
// A placement the API rejects degrades to the canonical marker form
// (v1.WrapSystemMarker) as a user turn instead of 400ing upstream.

// systemFallbackUserMsg rewrites a role:system wire message into the
// marker-wrapped user-turn form, preserving a cache breakpoint if the
// content was anchored (block form).
func systemFallbackUserMsg(m anthropicCanonMsg) anthropicCanonMsg {
	switch c := m.Content.(type) {
	case string:
		return anthropicCanonMsg{Role: "user", Content: v1.WrapSystemMarker(c)}
	case []map[string]any:
		for _, b := range c {
			if t, ok := b["text"].(string); ok {
				b["text"] = v1.WrapSystemMarker(t)
			}
		}
		return anthropicCanonMsg{Role: "user", Content: c}
	default:
		return anthropicCanonMsg{Role: "user", Content: c}
	}
}

// unwrapSystemUserTurn restores a marker-wrapped user turn back to a
// canonical system message. Only a message that is exactly one text part
// wholly enclosed in the markers qualifies — anything else stays user text.
func unwrapSystemUserTurn(toolResults []*v1.FunctionCallOutput, textParts []v1.Part) *v1.Message {
	if len(toolResults) != 0 || len(textParts) != 1 {
		return nil
	}
	tp, ok := textParts[0].(*v1.TextPart)
	if !ok {
		return nil
	}
	inner, ok := v1.UnwrapSystemMarker(tp.Text)
	if !ok {
		return nil
	}
	return &v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: inner}}}
}

// legalizeSystemPlacement enforces the API's placement rules for role:system
// wire messages: never first, immediately after a user turn, immediately
// before an assistant turn or the end of the array. Consecutive system
// messages count as one section. A section anywhere else converts to
// marker-wrapped user turns in place.
func legalizeSystemPlacement(msgs []anthropicCanonMsg) []anthropicCanonMsg {
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "system" {
			continue
		}
		j := i
		for j < len(msgs) && msgs[j].Role == "system" {
			j++
		}
		prevOK := i > 0 && msgs[i-1].Role == "user"
		nextOK := j == len(msgs) || msgs[j].Role == "assistant"
		if !prevOK || !nextOK {
			for k := i; k < j; k++ {
				msgs[k] = systemFallbackUserMsg(msgs[k])
			}
		}
		i = j - 1
	}
	return msgs
}

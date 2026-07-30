package anthropic

import (
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Mid-conversation system messages: canonical system/developer items that
// appear after conversation content stay positional as user turns wrapped in
// the canonical marker (v1.WrapSystemMarker) — works on every model and keeps
// the cached prefix untouched. Callers opt into real system authority per
// item via hoist (merged into the top-level system field at serialization).
// Inbound role:system wire messages parse to positional canonical system
// items; marker-wrapped user turns parse back to system items too, so the
// marker never surfaces as user text on replay.

// unwrapSystemUserTurn restores a marker-wrapped user turn back to a
// canonical system message. Only a message that is exactly one text part
// wholly enclosed in the markers qualifies — anything else stays user text,
// so quoted or embedded content is never promoted to system priority.
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

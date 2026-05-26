package v1

import "encoding/json"

// IsReasoningFrame reports whether a canonical SSE frame carries reasoning
// content — a reasoning item.started, a reasoning item.delta, or a
// reasoning item.completed. It lets a consumer time the reasoning span
// over a canonical stream without accumulating the content itself.
//
// Cheap by design: non-item frames return on the event-name switch
// without parsing, and item frames decode only the single discriminator
// field (item_type / kind / item.type), not the full payload.
func IsReasoningFrame(frame []byte) bool {
	event, data, ok := ParseSSEChunk(frame)
	if !ok {
		return false
	}
	return IsReasoningEvent(event, data)
}

// IsReasoningEvent is IsReasoningFrame for an already-parsed frame: event
// name plus payload. Callers that have decoded the SSE frame (the client's
// Recv loop) avoid re-parsing.
func IsReasoningEvent(event string, data []byte) bool {
	switch event {
	case EventItemStarted:
		var e struct {
			ItemType ItemType `json:"item_type"`
		}
		return json.Unmarshal(data, &e) == nil && e.ItemType == ItemTypeReasoning
	case EventItemDelta:
		var e struct {
			Kind DeltaKind `json:"kind"`
		}
		return json.Unmarshal(data, &e) == nil && e.Kind == DeltaKindReasoning
	case EventItemCompleted:
		var e struct {
			Item struct {
				Type ItemType `json:"type"`
			} `json:"item"`
		}
		return json.Unmarshal(data, &e) == nil && e.Item.Type == ItemTypeReasoning
	}
	return false
}

package openai

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// itemCompletedFrame converts a Responses output_item.done event into the
// canonical item.completed frame. ok is false when the event carries no item,
// the item fails to decode, or it has no canonical representation.
func (s *responsesToCanonicalStream) itemCompletedFrame(data []byte) (v1.SSEFrame, bool) {
	// Two-phase parse: extract output_index and the raw item bytes.
	// ResponsesOutputItemDoneEvent.Item is a ResponsesItem interface that
	// json.Unmarshal cannot populate — unmarshal the item bytes separately
	// via responsesUnmarshalItem which uses the "type" discriminator.
	var evHeader struct {
		OutputIndex int             `json:"output_index"`
		Item        json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &evHeader); err != nil {
		return v1.SSEFrame{}, false
	}
	if len(evHeader.Item) == 0 || string(evHeader.Item) == "null" {
		return v1.SSEFrame{}, false
	}
	wireItem, err := responsesUnmarshalItem(evHeader.Item)
	if err != nil {
		return v1.SSEFrame{}, false
	}
	ci, _ := responsesItemToCanonical(wireItem)
	if ci == nil {
		return v1.SSEFrame{}, false
	}
	// gpt-5.5's terminal reasoning item arrives with an empty summary (the
	// text came over reasoning_summary_text deltas). Backfill it from what we
	// accumulated so non-streaming consumers / logs see the thinking too.
	if r, ok := ci.(*v1.Reasoning); ok && len(r.Summary) == 0 {
		if acc := s.reasoningSummary[responsesItemID(wireItem)]; acc != "" {
			r.Summary = []v1.SummaryText{{Text: acc}}
		}
	}
	// A truncated (max_output_tokens) turn can close a function_call with
	// malformed argument JSON; downgrade to incomplete so the caller never
	// sees a runnable-looking call with broken args.
	if fc, ok := ci.(*v1.FunctionCall); ok && fc.Status == v1.StatusCompleted &&
		fc.Arguments != "" && !json.Valid([]byte(fc.Arguments)) {
		fc.Status = v1.StatusIncomplete
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: responsesItemID(wireItem),
		Index:  evHeader.OutputIndex,
		Item:   ci,
	})
	return v1.SSEFrame{Event: v1.EventItemCompleted, Data: completedData}, true
}

// responsesItemID extracts the ID field from a ResponsesItem via type assertion.
func responsesItemID(item ResponsesItem) string {
	switch v := item.(type) {
	case *ResponsesMessage:
		return v.ID
	case *ResponsesFunctionCall:
		return v.ID
	case *ResponsesReasoning:
		return v.ID
	case *ResponsesCustomToolCall:
		return v.ID
	default:
		return ""
	}
}

package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// NewToCanonicalStream returns a stateful per-stream function that converts one
// Responses SSE chunk into one or more canonical SSE chunks.
func (ResponsesTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &responsesToCanonicalStream{}
	return s.translate
}

// responsesToCanonicalStream converts Responses SSE events to canonical SSE frames.
// The Responses stream already uses item-based events closely aligned with canonical.
type responsesToCanonicalStream struct {
	responseID string
	model      string
	created    int64
	started    bool
	// reasoningSummary accumulates streamed reasoning-summary text per reasoning
	// item id, so the item's terminal output_item.done (which arrives with an
	// empty summary on gpt-5.5) can be backfilled with what was streamed.
	reasoningSummary map[string]string
}

func (s *responsesToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := ParseResponsesSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	var frames []v1.SSEFrame

	switch event {
	case ResponsesEventCreated:
		var ev ResponsesCreatedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("responses stream: created event: %w", err)
		}
		if ev.Response != nil {
			s.responseID = ev.Response.ID
			s.model = ev.Response.Model
			s.created = ev.Response.CreatedAt
		}
		if !s.started {
			s.started = true
			createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
				ID:    s.responseID,
				Model: s.model,
			})
			frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCreated, Data: createdData})
		}

	case ResponsesEventInProgress:
		// No canonical equivalent; ignore.

	case ResponsesEventOutputItemAdded:
		// Two-phase parse: extract output_index and the item's id+type from
		// flat fields (Item is a ResponsesItem interface; json.Unmarshal cannot
		// populate it without a custom dispatcher). Use the item's "type" field
		// directly to determine canonical ItemType.
		var evHeader struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data, &evHeader); err != nil {
			return nil, nil
		}
		if len(evHeader.Item) == 0 || string(evHeader.Item) == "null" {
			return nil, nil
		}
		var itemProbe struct {
			Type ResponsesItemType `json:"type"`
			ID   string            `json:"id"`
			Name string            `json:"name"`
		}
		if err := json.Unmarshal(evHeader.Item, &itemProbe); err != nil {
			return nil, nil
		}
		if itemProbe.ID == "" || itemProbe.Type == "" {
			return nil, nil
		}
		if !responsesCanonicalItemType(itemProbe.Type) {
			// Unmodeled item type (hosted-tool call): its output_item.done drops
			// at responsesItemToCanonical, so emitting item.started here would
			// orphan a started-without-completed. Skip the lifecycle entirely.
			return nil, nil
		}
		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   itemProbe.ID,
			ItemType: v1.ItemType(itemProbe.Type),
			// Name rides item.started for function_call items so downstream
			// serializers that emit the tool name at item-start (Anthropic) have it.
			Name:  itemProbe.Name,
			Index: evHeader.OutputIndex,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})

	case ResponsesEventOutputTextDelta:
		var ev ResponsesOutputTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindText,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	case ResponsesEventFunctionCallArgumentsDelta:
		var ev ResponsesFunctionCallArgumentsDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindArguments,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	case ResponsesEventReasoningTextDelta:
		var ev ResponsesReasoningTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindReasoning,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	// Reasoning summary deltas are the ONLY reasoning text a summary-mode model
	// streams as plaintext (raw reasoning_text is encrypted on gpt-5.5). Map them
	// to the same canonical reasoning delta so the thinking renders live, and
	// accumulate so the terminal reasoning item carries the summary too.
	case ResponsesEventReasoningSummaryTextDelta:
		var ev ResponsesReasoningSummaryTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		if s.reasoningSummary == nil {
			s.reasoningSummary = map[string]string{}
		}
		s.reasoningSummary[ev.ItemID] += ev.Delta
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindReasoning,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	// R-2: refusal deltas map to text deltas (canonical rule 9: refusal is text +
	// finish_reason, not a separate item type).
	case ResponsesEventRefusalDelta:
		var ev ResponsesRefusalDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindText,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	case ResponsesEventRefusalDone:
		// The done event carries no new content — the refusal text was streamed
		// via refusal.delta events above. The terminal finish_reason=refusal is
		// emitted by the response.completed/incomplete handler below.

	case ResponsesEventOutputItemDone:
		frame, ok := s.itemCompletedFrame(data)
		if !ok {
			return nil, nil
		}
		frames = append(frames, frame)

	case ResponsesEventCompleted, ResponsesEventIncomplete:
		// Parse the terminal response via the polymorphic-aware unmarshaler:
		// a plain json.Unmarshal into ResponsesResponse fails on the
		// []ResponsesItem interface `output`, and the swallowed error dropped
		// the whole terminal event — losing usage, finish_reason, and [DONE]
		// from every streamed cross-shape response.
		resp := parseStreamTerminalResponse(data)
		if resp == nil {
			return nil, nil
		}
		cr := responsesResponseToCanonical(resp)
		completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
			ID:           cr.ID,
			Status:       cr.Status,
			FinishReason: cr.FinishReason,
			Usage:        cr.Usage,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	// R-2: response.failed means the generation terminated with an error; emit
	// generation.completed with StatusFailed so the consumer isn't left hanging.
	case ResponsesEventFailed:
		resp := parseStreamTerminalResponse(data)
		if resp == nil {
			errData, _ := json.Marshal(v1.ErrorEvent{Code: "response_failed", Message: "response failed"})
			frames = append(frames, v1.SSEFrame{Event: v1.EventError, Data: errData})
			return marshalCanonicalFrames(frames), nil
		}
		cr := responsesResponseToCanonical(resp)
		completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
			ID:           cr.ID,
			Status:       v1.StatusFailed,
			FinishReason: cr.FinishReason,
			Usage:        cr.Usage,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	case ResponsesEventError:
		var ev ResponsesErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		errData, _ := json.Marshal(v1.ErrorEvent{
			Code:    ev.Code,
			Message: ev.Message,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventError, Data: errData})

	default:
		// Unhandled events fall into two classes, both intentionally dropped:
		//
		//  1. Redundant lifecycle/.done events (content_part.added, output_text.done,
		//     reasoning_summary_part.*, queued, in_progress) — carry nothing the 6
		//     canonical events don't already convey.
		//  2. Hosted-tool / annotation / audio events (web_search_call.*,
		//     file_search_call.*, code_interpreter_call.*, image_generation_call.*,
		//     mcp_call.*, custom_tool_call_input.*, output_text.annotation.added,
		//     audio.*) — no canonical representation exists yet.
		//
		// canonical: hosted-tool / annotation / audio stream events dropped — no
		// canonical event. Note mcp_call.failed / mcp_list_tools.failed carry an
		// error that is lost here; surfacing it as a fatal canonical error would
		// wrongly abort an otherwise-completing stream, so it waits for a
		// non-fatal canonical warning channel (deferred).
	}

	return marshalCanonicalFrames(frames), nil
}

// --- canonical → Responses stream ---

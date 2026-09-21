package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// NewFromCanonicalStream returns a stateful per-stream function that converts
// one canonical SSE chunk into one or more Responses SSE chunks.
func (ResponsesTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToResponsesStream{}
	return s.translate
}

// NewFromCanonicalStreamFor is NewFromCanonicalStream seeded with the request (v1.RequestAwareStream): a call of a tool the caller defined as `custom` must stream back as a custom_tool_call, and canonical marks no such difference — only the request's tool definitions do.
func (ResponsesTranslator) NewFromCanonicalStreamFor(req *v1.Request) func(chunk []byte) ([]byte, error) {
	s := &canonicalToResponsesStream{custom: newResponsesCustomLowering(req)}
	return s.translate
}

// --- request conversion helpers ---

// canonicalToResponsesStream converts canonical SSE frames to Responses SSE frames.
// This is the "from canonical" direction: used when serving Responses inbound callers
// whose upstream was translated through canonical.
type canonicalToResponsesStream struct {
	responseID    string
	model         string
	created       int64
	outputItems   map[string]responsesStreamItem // itemID → state
	outputIndex   map[string]int                 // itemID → outputIndex
	closedItems   []ResponsesItem
	lifecycleDone bool
	seq           int // next sequence_number; per-stream state, never on the Translator (rule 6)
	custom        *responsesCustomLowering
}

type responsesStreamItem struct {
	itemType    v1.ItemType
	outputIndex int
	textBuf     string
	argsBuf     string
	callID      string
	name        string
	custom      bool // this call's tool was defined as `custom` on the request
}

func (s *canonicalToResponsesStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	if s.outputItems == nil {
		s.outputItems = make(map[string]responsesStreamItem)
		s.outputIndex = make(map[string]int)
	}

	var frames []ResponsesSSEFrame

	switch event {
	case v1.EventGenerationCreated:
		var ev v1.GenerationCreatedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		s.responseID = ev.ID
		s.model = ev.Model
		s.created = time.Now().Unix()

		stub := &ResponsesResponse{
			ID:        s.responseID,
			Object:    "response",
			CreatedAt: s.created,
			Model:     s.model,
			Status:    ResponsesStatusInProgress,
			Output:    []ResponsesItem{},
		}
		createdData, _ := json.Marshal(ResponsesCreatedEvent{Response: stub})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventCreated, Data: createdData})
		inProgData, _ := json.Marshal(ResponsesInProgressEvent{Response: stub})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventInProgress, Data: inProgData})

	case v1.EventItemStarted:
		var ev v1.ItemStartedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		// R-3: capture name from item.started so function call events carry it.
		// Use itemID as provisional callID — the real callID arrives on item.completed.
		// Custom-ness is decided here, from the name alone, and kept for the item's whole lifecycle: added and done must describe the same item type, and an upstream that omits the name at item.started leaves nothing else to match on.
		s.outputItems[ev.ItemID] = responsesStreamItem{
			itemType:    ev.ItemType,
			outputIndex: ev.Index,
			name:        ev.Name,
			callID:      ev.ItemID, // provisional; overwritten from item.completed payload
			custom:      ev.ItemType == v1.ItemTypeFunctionCall && s.custom.isCustomName(ev.Name),
		}
		s.outputIndex[ev.ItemID] = ev.Index
		switch ev.ItemType {
		case v1.ItemTypeMessage:
			msgItem := &ResponsesMessage{
				ID:     ev.ItemID,
				Role:   ResponsesRoleAssistant,
				Status: ResponsesStatusInProgress,
			}
			addedData, _ := json.Marshal(ResponsesItemAddedEvent{OutputIndex: ev.Index, Item: msgItem})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemAdded, Data: addedData})
			partData, _ := json.Marshal(ResponsesContentPartAddedEvent{
				ItemID:       ev.ItemID,
				OutputIndex:  ev.Index,
				ContentIndex: 0,
				Part:         &ResponsesOutputTextPart{Text: ""},
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventContentPartAdded, Data: partData})

		case v1.ItemTypeFunctionCall:
			var callItem ResponsesItem = &ResponsesFunctionCall{
				ID:     ev.ItemID,
				Name:   ev.Name,
				Status: ResponsesStatusInProgress,
			}
			if s.outputItems[ev.ItemID].custom {
				callItem = &ResponsesCustomToolCall{
					ID:     ev.ItemID,
					Name:   ev.Name,
					Status: ResponsesStatusInProgress,
				}
			}
			addedData, _ := json.Marshal(ResponsesItemAddedEvent{OutputIndex: ev.Index, Item: callItem})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemAdded, Data: addedData})

		case v1.ItemTypeReasoning:
			rItem := &ResponsesReasoning{
				ID:     ev.ItemID,
				Status: ResponsesStatusInProgress,
			}
			addedData, _ := json.Marshal(ResponsesItemAddedEvent{OutputIndex: ev.Index, Item: rItem})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemAdded, Data: addedData})
		}

	case v1.EventItemDelta:
		var ev v1.ItemDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		st, ok := s.outputItems[ev.ItemID]
		if !ok {
			return nil, nil
		}
		switch ev.Kind {
		case v1.DeltaKindText:
			st.textBuf += ev.Delta
			s.outputItems[ev.ItemID] = st
			deltaData, _ := json.Marshal(ResponsesOutputTextDeltaEvent{
				ItemID:       ev.ItemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Delta:        ev.Delta,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputTextDelta, Data: deltaData})

		case v1.DeltaKindArguments:
			st.argsBuf += ev.Delta
			s.outputItems[ev.ItemID] = st
			if st.custom {
				// A custom tool's input is the freeform string inside the JSON arguments, which only becomes readable once the object closes — so the whole input ships as one delta from itemDoneFrames.
				break
			}
			// R-3: emit callID and name from stored per-item state.
			deltaData, _ := json.Marshal(ResponsesFunctionCallArgumentsDeltaEvent{
				ItemID:      ev.ItemID,
				OutputIndex: st.outputIndex,
				CallID:      st.callID,
				Delta:       ev.Delta,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventFunctionCallArgumentsDelta, Data: deltaData})

		case v1.DeltaKindReasoning:
			st.textBuf += ev.Delta
			s.outputItems[ev.ItemID] = st
			deltaData, _ := json.Marshal(ResponsesReasoningTextDeltaEvent{
				ItemID:       ev.ItemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Delta:        ev.Delta,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventReasoningTextDelta, Data: deltaData})
		}

	case v1.EventItemCompleted:
		doneFrames, ok := s.itemDoneFrames(data)
		if !ok {
			return nil, nil
		}
		frames = append(frames, doneFrames...)

	case v1.EventGenerationCompleted:
		var ev v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}

		finalResp := &ResponsesResponse{
			ID:        s.responseID,
			Object:    "response",
			CreatedAt: s.created,
			Model:     s.model,
			Output:    append([]ResponsesItem{}, s.closedItems...),
		}
		finalResp.Status, finalResp.IncompleteDetails = canonicalResponsesStatus(ev.Status, ev.FinishReason, nil)
		if ev.Usage != nil {
			finalResp.Usage = canonicalUsageToResponses(ev.Usage)
		}

		var finalEvent string
		if finalResp.Status == ResponsesStatusIncomplete {
			finalEvent = ResponsesEventIncomplete
		} else {
			finalEvent = ResponsesEventCompleted
		}
		completedData, _ := json.Marshal(ResponsesCompletedEvent{Response: finalResp})
		frames = append(frames, ResponsesSSEFrame{Event: finalEvent, Data: completedData})

	case v1.EventError:
		var ev v1.ErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		errData, _ := json.Marshal(ResponsesErrorEvent{Code: ev.Code, Message: ev.Message})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventError, Data: errData})
	}

	return s.marshalFrames(frames), nil
}

// marshalFrames stamps the stream envelope onto each frame and serializes them to wire bytes.
func (s *canonicalToResponsesStream) marshalFrames(frames []ResponsesSSEFrame) []byte {
	if len(frames) == 0 {
		return nil
	}
	var buf []byte
	for _, f := range frames {
		f.Data = s.stampEnvelope(f.Event, f.Data)
		buf = append(buf, f.Bytes()...)
	}
	return buf
}

// stampEnvelope prefixes the two fields every Responses event carries: "type", which repeats the event: line because clients dispatch on the JSON and not the SSE field, and "sequence_number", a per-stream counter from 0 that lets a consumer order events and spot a gap.
//
// Prefixing rather than re-marshaling keeps the rest of the payload byte-identical to what the event structs produced.
func (s *canonicalToResponsesStream) stampEnvelope(event string, data []byte) []byte {
	body := bytes.TrimSpace(data)
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return data
	}
	head := fmt.Sprintf(`{"type":%q,"sequence_number":%d`, event, s.seq)
	s.seq++
	if len(body) == 2 { // "{}" — nothing to join onto
		return []byte(head + "}")
	}
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	out = append(out, ',')
	return append(out, body[1:]...)
}

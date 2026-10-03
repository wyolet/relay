package openai

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// itemDoneFrames renders the Responses closing events (text/arguments done,
// content-part done, output_item.done) for one canonical item.completed event.
// ok is false when the event is unparseable or names an item this stream never
// started — the caller then emits nothing.
func (s *canonicalToResponsesStream) itemDoneFrames(data []byte) ([]ResponsesSSEFrame, bool) {
	// Two-phase parse: extract item_id and index from a flat struct first
	// (the Item field is a v1.Item interface that json.Unmarshal cannot
	// populate without a custom dispatcher — the full item is not needed
	// because per-stream state in s.outputItems already holds type + buffers).
	var evHeader struct {
		ItemID string `json:"item_id"`
		Index  int    `json:"index"`
	}
	if err := json.Unmarshal(data, &evHeader); err != nil {
		return nil, false
	}
	st, ok := s.outputItems[evHeader.ItemID]
	if !ok {
		return nil, false
	}
	itemID := evHeader.ItemID

	var frames []ResponsesSSEFrame

	switch st.itemType {
	case v1.ItemTypeMessage:
		finalPart := &ResponsesOutputTextPart{Text: st.textBuf}
		textDoneData, _ := json.Marshal(ResponsesOutputTextDoneEvent{
			ItemID:       itemID,
			OutputIndex:  st.outputIndex,
			ContentIndex: 0,
			Text:         st.textBuf,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputTextDone, Data: textDoneData})
		partDoneData, _ := json.Marshal(ResponsesContentPartDoneEvent{
			ItemID:       itemID,
			OutputIndex:  st.outputIndex,
			ContentIndex: 0,
			Part:         finalPart,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventContentPartDone, Data: partDoneData})
		finalMsg := &ResponsesMessage{
			ID:      itemID,
			Role:    ResponsesRoleAssistant,
			Status:  ResponsesStatusCompleted,
			Content: []ResponsesPart{finalPart},
		}
		itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
			OutputIndex: st.outputIndex,
			Item:        finalMsg,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
		s.closedItems = append(s.closedItems, finalMsg)

	case v1.ItemTypeFunctionCall:
		// R-3: patch callID and name from the completed item payload if available,
		// falling back to per-stream state populated from item.started.
		callID := st.callID
		name := st.name
		var fcProbe struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		var evItemRaw struct {
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(data, &evItemRaw) == nil && len(evItemRaw.Item) > 0 {
			if json.Unmarshal(evItemRaw.Item, &fcProbe) == nil {
				if fcProbe.CallID != "" {
					callID = fcProbe.CallID
				}
				if fcProbe.Name != "" {
					name = fcProbe.Name
				}
				if fcProbe.Arguments != "" {
					st.argsBuf = fcProbe.Arguments
				}
			}
		}
		if st.custom {
			input := responsesCustomToolInput(st.argsBuf)
			deltaData, _ := json.Marshal(ResponsesCustomToolCallInputDeltaEvent{
				ItemID:      itemID,
				OutputIndex: st.outputIndex,
				Delta:       input,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventCustomToolCallInputDelta, Data: deltaData})
			doneData, _ := json.Marshal(ResponsesCustomToolCallInputDoneEvent{
				ItemID:      itemID,
				OutputIndex: st.outputIndex,
				Input:       input,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventCustomToolCallInputDone, Data: doneData})
			finalCall := &ResponsesCustomToolCall{
				ID:     itemID,
				CallID: callID,
				Name:   name,
				Input:  input,
				Status: ResponsesStatusCompleted,
			}
			itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
				OutputIndex: st.outputIndex,
				Item:        finalCall,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
			s.closedItems = append(s.closedItems, finalCall)
			break
		}
		argsDoneData, _ := json.Marshal(ResponsesFunctionCallArgumentsDoneEvent{
			ItemID:      itemID,
			OutputIndex: st.outputIndex,
			CallID:      callID,
			Arguments:   st.argsBuf,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventFunctionCallArgumentsDone, Data: argsDoneData})
		finalFC := &ResponsesFunctionCall{
			ID:        itemID,
			CallID:    callID,
			Name:      name,
			Arguments: st.argsBuf,
			Status:    ResponsesStatusCompleted,
		}
		itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
			OutputIndex: st.outputIndex,
			Item:        finalFC,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
		s.closedItems = append(s.closedItems, finalFC)

	case v1.ItemTypeReasoning:
		textDoneData, _ := json.Marshal(ResponsesReasoningTextDoneEvent{
			ItemID:       itemID,
			OutputIndex:  st.outputIndex,
			ContentIndex: 0,
			Text:         st.textBuf,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventReasoningTextDone, Data: textDoneData})
		finalR := &ResponsesReasoning{
			ID:      itemID,
			Status:  ResponsesStatusCompleted,
			Summary: []ResponsesSummaryText{{Text: st.textBuf}},
		}
		itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
			OutputIndex: st.outputIndex,
			Item:        finalR,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
		s.closedItems = append(s.closedItems, finalR)
	}

	return frames, true
}

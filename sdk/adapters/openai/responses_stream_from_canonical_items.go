package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// itemDoneFrames renders the Responses closing events (text/arguments done,
// content-part done, output_item.done) for one canonical item.completed event.
// It renders nothing for an item this stream never started: its item.started
// was already rejected, so there is no opened item to close.
func (s *canonicalToResponsesStream) itemDoneFrames(data []byte) ([]ResponsesSSEFrame, error) {
	// Two-phase parse: extract item_id and index from a flat struct first
	// (the Item field is a v1.Item interface that json.Unmarshal cannot
	// populate without a custom dispatcher — the full item is not needed
	// because per-stream state in s.outputItems already holds type + buffers).
	var evHeader struct {
		ItemID string          `json:"item_id"`
		Index  int             `json:"index"`
		Item   json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &evHeader); err != nil {
		return nil, fmt.Errorf("responses from_canonical stream: item.completed: %w", err)
	}
	st, ok := s.outputItems[evHeader.ItemID]
	if !ok {
		return nil, nil
	}
	itemID := evHeader.ItemID
	// The completed item's status is passed through as is: an item cut short stays incomplete, and one the source sent without a status gets none.
	var done struct {
		Status       ResponsesStatus `json:"status"`
		Content      string          `json:"content"`
		ProviderData json.RawMessage `json:"provider_data"`
	}
	if len(evHeader.Item) > 0 {
		_ = json.Unmarshal(evHeader.Item, &done)
	}

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
			Status:  done.Status,
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
		if len(evHeader.Item) > 0 {
			if json.Unmarshal(evHeader.Item, &fcProbe) == nil {
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
				Status: done.Status,
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
			Status:    done.Status,
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
			Status:  done.Status,
			Summary: []ResponsesSummaryText{{Text: st.textBuf}},
		}
		if done.Content != "" {
			finalR.Content = []ResponsesReasoningText{{Text: done.Content}}
		}
		// Same-vendor round trip: encrypted_content rides provider_data, as in responsesItemFromCanonical.
		var pd struct {
			EncryptedContent string `json:"encrypted_content"`
		}
		if json.Unmarshal(done.ProviderData, &pd) == nil {
			finalR.EncryptedContent = pd.EncryptedContent
		}
		itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
			OutputIndex: st.outputIndex,
			Item:        finalR,
		})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
		s.closedItems = append(s.closedItems, finalR)
	}

	return frames, nil
}

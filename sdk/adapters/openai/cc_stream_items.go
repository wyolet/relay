package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func (s *ccToCanonicalStream) handleReasoningDelta(text, field string) ([]v1.SSEFrame, error) {
	var frames []v1.SSEFrame

	if s.reasoningItem == nil {
		ti := &ccStreamItem{
			kind:           ccStreamKindReasoning,
			outputIndex:    s.nextIndex,
			itemID:         fmt.Sprintf("rs_%d", s.nextIndex),
			reasoningField: field,
		}
		s.nextIndex++
		s.reasoningItem = ti

		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   ti.itemID,
			ItemType: v1.ItemTypeReasoning,
			Index:    ti.outputIndex,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})
	}

	s.reasoningItem.textBuf += text
	deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
		ItemID: s.reasoningItem.itemID,
		Index:  s.reasoningItem.outputIndex,
		Kind:   v1.DeltaKindReasoning,
		Delta:  text,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})
	return frames, nil
}

func (s *ccToCanonicalStream) handleTextDelta(text string) ([]v1.SSEFrame, error) {
	var frames []v1.SSEFrame

	if s.msgItem == nil {
		// Close reasoning first if open.
		if s.reasoningItem != nil {
			frames = append(frames, s.closeReasoningItem(s.reasoningItem)...)
			s.reasoningItem = nil
		}

		ti := &ccStreamItem{
			kind:        ccStreamKindMessage,
			outputIndex: s.nextIndex,
			itemID:      fmt.Sprintf("msg_%d", s.nextIndex),
		}
		s.nextIndex++
		s.msgItem = ti

		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   ti.itemID,
			ItemType: v1.ItemTypeMessage,
			Index:    ti.outputIndex,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})
	}

	s.msgItem.textBuf += text
	deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
		ItemID: s.msgItem.itemID,
		Index:  s.msgItem.outputIndex,
		Kind:   v1.DeltaKindText,
		Delta:  text,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})
	return frames, nil
}

func (s *ccToCanonicalStream) handleToolCallDelta(tc ToolCallChunk) ([]v1.SSEFrame, error) {
	var frames []v1.SSEFrame

	// Close open message item.
	if s.msgItem != nil {
		frames = append(frames, s.closeMsgItem(s.msgItem)...)
		s.msgItem = nil
	}
	// Close open reasoning item.
	if s.reasoningItem != nil {
		frames = append(frames, s.closeReasoningItem(s.reasoningItem)...)
		s.reasoningItem = nil
	}

	ti, exists := s.toolItems[tc.Index]
	if !exists {
		itemID := tc.ID
		if itemID == "" {
			itemID = fmt.Sprintf("fc_%d", s.nextIndex)
		}
		name := ""
		if tc.Function != nil {
			name = tc.Function.Name
		}
		ti = &ccStreamItem{
			kind:        ccStreamKindToolCall,
			outputIndex: s.nextIndex,
			itemID:      itemID,
			callID:      tc.ID,
			name:        name,
		}
		s.nextIndex++
		s.toolItems[tc.Index] = ti

		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   ti.itemID,
			ItemType: v1.ItemTypeFunctionCall,
			Index:    ti.outputIndex,
			Name:     ti.name,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})
	}

	// Some OpenAI-compatible upstreams (vLLM, aggregators) send the id-only
	// fragment first and the function name in a later one; backfill so the
	// completed item carries it even when item.started went out nameless.
	if ti.name == "" && tc.Function != nil && tc.Function.Name != "" {
		ti.name = tc.Function.Name
	}
	if tc.Function != nil && tc.Function.Arguments != "" {
		ti.argsBuf += tc.Function.Arguments
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ti.itemID,
			Index:  ti.outputIndex,
			Kind:   v1.DeltaKindArguments,
			Delta:  tc.Function.Arguments,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})
	}

	return frames, nil
}

func (s *ccToCanonicalStream) closeMsgItem(ti *ccStreamItem) []v1.SSEFrame {
	finalMsg := &v1.Message{
		ID:     ti.itemID,
		Role:   v1.RoleAssistant,
		Status: v1.StatusCompleted,
	}
	if ti.textBuf != "" {
		finalMsg.Content = []v1.Part{&v1.OutputTextPart{Text: ti.textBuf}}
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: ti.itemID,
		Index:  ti.outputIndex,
		Item:   finalMsg,
	})
	return []v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}
}

func (s *ccToCanonicalStream) closeReasoningItem(ti *ccStreamItem) []v1.SSEFrame {
	finalItem := &v1.Reasoning{
		ID:           ti.itemID,
		Content:      ti.textBuf,
		Status:       v1.StatusCompleted,
		ProviderData: ccReasoningProviderDataJSON(ti.reasoningField),
	}
	if ti.textBuf != "" {
		finalItem.Summary = []v1.SummaryText{{Text: ti.textBuf}}
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: ti.itemID,
		Index:  ti.outputIndex,
		Item:   finalItem,
	})
	return []v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}
}

func (s *ccToCanonicalStream) closeToolItem(ti *ccStreamItem) []v1.SSEFrame {
	// OpenAI streams tool args unbuffered, so a length-truncated turn can
	// close with malformed argument JSON. Mark it incomplete rather than
	// hand the caller a runnable-looking call. Empty args stay completed
	// (no-arg tools may stream zero argument fragments).
	status := v1.StatusCompleted
	if ti.argsBuf != "" && !json.Valid([]byte(ti.argsBuf)) {
		status = v1.StatusIncomplete
	}
	finalItem := &v1.FunctionCall{
		ID:        ti.itemID,
		CallID:    ti.callID,
		Name:      ti.name,
		Arguments: ti.argsBuf,
		Status:    status,
	}
	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: ti.itemID,
		Index:  ti.outputIndex,
		Item:   finalItem,
	})
	return []v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}
}

package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// NewToCanonicalStream returns a stateful per-stream function that converts one
// CC SSE chunk into one or more canonical SSE chunks.
func (CCTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &ccToCanonicalStream{}
	return s.translate
}

type ccStreamItemKind int

const (
	ccStreamKindMessage ccStreamItemKind = iota
	ccStreamKindToolCall
	ccStreamKindReasoning
)

type ccStreamItem struct {
	kind        ccStreamItemKind
	outputIndex int
	itemID      string
	callID      string
	name        string
	textBuf     string
	argsBuf     string
	// reasoningField records the wire field name (reasoning|reasoning_content)
	// for reasoning items, preserved into the canonical item's provider_data.
	reasoningField string
}

// ccToCanonicalStream is a stateful CC SSE → canonical SSE translator.
type ccToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	nextIndex        int
	msgItem          *ccStreamItem
	reasoningItem    *ccStreamItem
	toolItems        map[int]*ccStreamItem
	lastUsage        *Usage
	lifecycleEmitted bool
	status           v1.Status
	finishReason     v1.FinishReason
	errorEmitted     bool
}

func ccStreamErrorFrame(data []byte) (v1.SSEFrame, bool) {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return v1.SSEFrame{}, false
	}
	errRaw := bytes.TrimSpace(probe.Error)
	if len(errRaw) == 0 || bytes.Equal(errRaw, []byte("null")) {
		return v1.SSEFrame{}, false
	}

	var ccErr struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(errRaw, &ccErr)

	code := ccErr.Type
	if code == "" && len(ccErr.Code) > 0 {
		var codeString string
		if err := json.Unmarshal(ccErr.Code, &codeString); err == nil {
			code = codeString
		} else {
			code = string(bytes.TrimSpace(ccErr.Code))
		}
	}
	msg := ccErr.Message
	if msg == "" {
		msg = string(data)
	}
	errData, _ := json.Marshal(v1.ErrorEvent{
		Code:    code,
		Message: msg,
	})
	return v1.SSEFrame{Event: v1.EventError, Data: errData}, true
}

func (s *ccToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	// Parse the CC SSE chunk.
	_, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}
	if s.errorEmitted {
		return nil, nil
	}

	if bytes.Equal(data, []byte("[DONE]")) {
		return s.handleDone()
	}

	if errFrame, ok := ccStreamErrorFrame(data); ok {
		s.errorEmitted = true
		return marshalCanonicalFrames([]v1.SSEFrame{errFrame}), nil
	}

	var ccChunk ChatStreamChunk
	if err := json.Unmarshal(data, &ccChunk); err != nil {
		return nil, fmt.Errorf("cc stream: parse chunk: %w", err)
	}

	if ccChunk.Usage != nil {
		s.lastUsage = ccChunk.Usage
	}

	var frames []v1.SSEFrame

	if !s.lifecycleEmitted {
		s.responseID = ccChunk.ID
		s.model = ccChunk.Model
		s.created = ccChunk.Created
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		if s.responseID == "" {
			s.responseID = fmt.Sprintf("resp_%d", s.created)
		}
		if s.toolItems == nil {
			s.toolItems = make(map[int]*ccStreamItem)
		}
		// Emit generation.created
		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
			ID:    s.responseID,
			Model: s.model,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCreated, Data: createdData})
		s.lifecycleEmitted = true
	}

	if len(ccChunk.Choices) == 0 {
		return marshalCanonicalFrames(frames), nil
	}

	ch := ccChunk.Choices[0]
	delta := ch.Delta

	// finish_reason arrives on the terminal chunk (separate from deltas); capture
	// it so handleDone emits the real reason instead of a hardcoded "stop".
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		s.status, s.finishReason, _ = ccFinishReasonToCanonical(*ch.FinishReason)
	}

	// Reasoning text (Ollama "reasoning" or o-series "reasoning_content").
	if rc, field := ccExtractReasoningContent(data); rc != "" {
		rf, err := s.handleReasoningDelta(rc, field)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Text content.
	if delta.Content != nil && *delta.Content != "" {
		rf, err := s.handleTextDelta(*delta.Content)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Tool calls.
	for _, tc := range delta.ToolCalls {
		rf, err := s.handleToolCallDelta(tc)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Refusal content.
	if delta.Refusal != nil && *delta.Refusal != "" {
		// Refusal in streaming: treat as text delta with finish_reason=refusal on completion.
		// Map to text delta here; the completed event will carry finish_reason=refusal.
		rf, err := s.handleTextDelta(*delta.Refusal)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	return marshalCanonicalFrames(frames), nil
}

func (s *ccToCanonicalStream) handleDone() ([]byte, error) {
	var frames []v1.SSEFrame

	// Close open reasoning item.
	if s.reasoningItem != nil {
		f := s.closeReasoningItem(s.reasoningItem)
		frames = append(frames, f...)
		s.reasoningItem = nil
	}

	// Close open message item.
	if s.msgItem != nil {
		f := s.closeMsgItem(s.msgItem)
		frames = append(frames, f...)
		s.msgItem = nil
	}

	// Close open tool call items.
	if len(s.toolItems) > 0 {
		type toolItemEntry struct {
			key  int
			item *ccStreamItem
		}
		toolItems := make([]toolItemEntry, 0, len(s.toolItems))
		for key, ti := range s.toolItems {
			toolItems = append(toolItems, toolItemEntry{key: key, item: ti})
		}
		sort.Slice(toolItems, func(i, j int) bool {
			if toolItems[i].item.outputIndex == toolItems[j].item.outputIndex {
				return toolItems[i].key < toolItems[j].key
			}
			return toolItems[i].item.outputIndex < toolItems[j].item.outputIndex
		})
		for _, entry := range toolItems {
			f := s.closeToolItem(entry.item)
			frames = append(frames, f...)
			delete(s.toolItems, entry.key)
		}
	}

	// generation.completed
	var u usage.Tokens
	if s.lastUsage != nil {
		u = ccUsageToCanonical(s.lastUsage)
	}
	status, finish := s.status, s.finishReason
	if finish == "" {
		status, finish = v1.StatusCompleted, v1.FinishReasonStop
	}
	completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
		ID:           s.responseID,
		Status:       status,
		FinishReason: finish,
		Usage:        u,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	return marshalCanonicalFrames(frames), nil
}

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

package cctranslator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

// SSEFrame is one server-sent event ready for the wire.
// The caller is responsible for adding the SSE envelope ("event: …\ndata: …\n\n").
type SSEFrame struct {
	Event string // one of the responses.Event* constants
	Data  []byte // JSON-marshaled event payload
}

// itemKind distinguishes which output item type a tracked slot represents.
type itemKind int

const (
	kindMessage  itemKind = iota
	kindToolCall          // function_call
	kindReasoning
)

// trackedItem holds in-flight state for one output item during streaming.
type trackedItem struct {
	kind        itemKind
	outputIndex int
	itemID      string
	callID      string // tool_call only
	name        string // function name for tool_call items
	textBuf     string // accumulated text (message or reasoning)
	argsBuf     string // accumulated arguments (tool_call only)
}

// Stream is a stateful per-stream translator that converts CC SSE chunks
// to Responses SSE frames. Create one per upstream response stream.
type Stream struct {
	responseID string
	model      string
	created    int64

	// Output item tracking
	closedItems []responses.Item // items closed in emission order, for final response reconstruction
	nextIndex   int              // next output_index to assign

	// Per-item open state
	msgItem       *trackedItem // currently open message item (nil if none)
	reasoningItem *trackedItem // currently open reasoning item (nil if none)

	// Tool call state: CC tool_calls[i].index → tracked item
	toolItems map[int]*trackedItem

	// Usage from last chunk that carried it
	lastUsage *openai.Usage

	// Whether lifecycle events (created/in_progress) have been emitted
	lifecycleEmitted bool
}

// NewStream returns a fresh Stream ready to process CC chunks.
func NewStream() *Stream {
	return &Stream{
		toolItems: make(map[int]*trackedItem),
	}
}

// Translate processes one CC SSE chunk and returns zero or more Responses
// SSEFrames. On [DONE], it closes any open items and emits the final
// response.completed event.
func (s *Stream) Translate(ccChunk []byte) ([]SSEFrame, error) {
	_, data, ok := parseSSEChunk(ccChunk)
	if !ok {
		return nil, nil
	}

	if bytes.Equal(data, []byte("[DONE]")) {
		return s.handleDone()
	}

	var chunk openai.ChatStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("cc stream: parse chunk: %w", err)
	}

	if chunk.Usage != nil {
		s.lastUsage = chunk.Usage
	}

	var frames []SSEFrame

	// Initialize on first chunk.
	if !s.lifecycleEmitted {
		s.responseID = chunk.ID
		s.model = chunk.Model
		s.created = chunk.Created
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		if s.responseID == "" {
			s.responseID = "resp_" + fmt.Sprintf("%d", s.created)
		}
		lf, err := s.emitLifecycle()
		if err != nil {
			return nil, err
		}
		frames = append(frames, lf...)
		s.lifecycleEmitted = true
	}

	if len(chunk.Choices) == 0 {
		return frames, nil
	}

	ch := chunk.Choices[0]
	delta := ch.Delta

	// Reasoning content (non-standard but emitted by some o-series upstreams).
	if rc := extractReasoningContent(data); rc != "" {
		rf, err := s.handleReasoningDelta(rc)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Text content.
	if delta.Content != nil {
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

	return frames, nil
}

// handleDone closes any open items and emits the final response.completed.
func (s *Stream) handleDone() ([]SSEFrame, error) {
	var frames []SSEFrame

	// Close open reasoning item.
	if s.reasoningItem != nil {
		cf, err := s.closeReasoningItem(s.reasoningItem)
		if err != nil {
			return nil, err
		}
		frames = append(frames, cf...)
		s.reasoningItem = nil
	}

	// Close open message item.
	if s.msgItem != nil {
		cf, err := s.closeMsgItem(s.msgItem)
		if err != nil {
			return nil, err
		}
		frames = append(frames, cf...)
		s.msgItem = nil
	}

	// Close open tool call items.
	for idx, ti := range s.toolItems {
		cf, err := s.closeToolItem(ti)
		if err != nil {
			return nil, err
		}
		frames = append(frames, cf...)
		delete(s.toolItems, idx)
	}

	// Reconstruct full response for completed event.
	resp := s.buildFinalResponse()
	var finalEvent string
	switch resp.Status {
	case responses.StatusIncomplete:
		finalEvent = responses.EventIncomplete
	default:
		finalEvent = responses.EventCompleted
	}

	payload := &responses.CompletedEvent{Response: resp}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cc stream: marshal completed event: %w", err)
	}
	frames = append(frames, SSEFrame{Event: finalEvent, Data: b})

	return frames, nil
}

// emitLifecycle emits response.created then response.in_progress.
func (s *Stream) emitLifecycle() ([]SSEFrame, error) {
	stub := &responses.Response{
		ID:        s.responseID,
		Object:    "response",
		CreatedAt: s.created,
		Model:     s.model,
		Status:    responses.StatusInProgress,
		Output:    []responses.Item{},
	}
	createdPayload, err := json.Marshal(&responses.CreatedEvent{Response: stub})
	if err != nil {
		return nil, err
	}
	inProgressPayload, err := json.Marshal(&responses.InProgressEvent{Response: stub})
	if err != nil {
		return nil, err
	}
	return []SSEFrame{
		{Event: responses.EventCreated, Data: createdPayload},
		{Event: responses.EventInProgress, Data: inProgressPayload},
	}, nil
}

// handleReasoningDelta opens a reasoning item on first delta and emits
// reasoning_text.delta.
func (s *Stream) handleReasoningDelta(text string) ([]SSEFrame, error) {
	var frames []SSEFrame

	if s.reasoningItem == nil {
		// First reasoning chunk: close any open msg item (shouldn't happen, but safe).
		if s.msgItem != nil {
			cf, err := s.closeMsgItem(s.msgItem)
			if err != nil {
				return nil, err
			}
			frames = append(frames, cf...)
			s.msgItem = nil
		}

		ti := &trackedItem{
			kind:        kindReasoning,
			outputIndex: s.nextIndex,
			itemID:      fmt.Sprintf("rs_%d", s.nextIndex),
		}
		s.nextIndex++
		s.reasoningItem = ti

		// response.output_item.added for reasoning
		reasoningItem := &responses.Reasoning{ID: ti.itemID, Status: responses.StatusInProgress}
		addedPayload, err := json.Marshal(&responses.ItemAddedEvent{
			OutputIndex: ti.outputIndex,
			Item:        reasoningItem,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, SSEFrame{Event: responses.EventOutputItemAdded, Data: addedPayload})
	}

	s.reasoningItem.textBuf += text

	delta, err := json.Marshal(&responses.ReasoningTextDeltaEvent{
		ItemID:       s.reasoningItem.itemID,
		OutputIndex:  s.reasoningItem.outputIndex,
		ContentIndex: 0,
		Delta:        text,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventReasoningTextDelta, Data: delta})

	return frames, nil
}

// handleTextDelta opens a message item + content part on first delta, and
// emits output_text.delta for subsequent ones.
func (s *Stream) handleTextDelta(text string) ([]SSEFrame, error) {
	var frames []SSEFrame

	if text == "" {
		return nil, nil
	}

	if s.msgItem == nil {
		// Close reasoning item first if open.
		if s.reasoningItem != nil {
			cf, err := s.closeReasoningItem(s.reasoningItem)
			if err != nil {
				return nil, err
			}
			frames = append(frames, cf...)
			s.reasoningItem = nil
		}
		// Close any tool items (shouldn't mix, but be safe).
		for idx, ti := range s.toolItems {
			cf, err := s.closeToolItem(ti)
			if err != nil {
				return nil, err
			}
			frames = append(frames, cf...)
			delete(s.toolItems, idx)
		}

		ti := &trackedItem{
			kind:        kindMessage,
			outputIndex: s.nextIndex,
			itemID:      fmt.Sprintf("msg_%d", s.nextIndex),
		}
		s.nextIndex++
		s.msgItem = ti

		// response.output_item.added
		msgItem := &responses.Message{
			ID:   ti.itemID,
			Role: responses.RoleAssistant,
		}
		addedPayload, err := json.Marshal(&responses.ItemAddedEvent{
			OutputIndex: ti.outputIndex,
			Item:        msgItem,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, SSEFrame{Event: responses.EventOutputItemAdded, Data: addedPayload})

		// response.content_part.added
		partPayload, err := json.Marshal(&responses.ContentPartAddedEvent{
			ItemID:       ti.itemID,
			OutputIndex:  ti.outputIndex,
			ContentIndex: 0,
			Part:         &responses.OutputTextPart{Text: ""},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, SSEFrame{Event: responses.EventContentPartAdded, Data: partPayload})
	}

	s.msgItem.textBuf += text

	deltaPayload, err := json.Marshal(&responses.OutputTextDeltaEvent{
		ItemID:       s.msgItem.itemID,
		OutputIndex:  s.msgItem.outputIndex,
		ContentIndex: 0,
		Delta:        text,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventOutputTextDelta, Data: deltaPayload})

	return frames, nil
}

// handleToolCallDelta opens a function_call item on first delta for a given
// tool call index and emits function_call_arguments.delta.
func (s *Stream) handleToolCallDelta(tc openai.ToolCallChunk) ([]SSEFrame, error) {
	var frames []SSEFrame

	// Close open message item before starting tool calls.
	if s.msgItem != nil {
		cf, err := s.closeMsgItem(s.msgItem)
		if err != nil {
			return nil, err
		}
		frames = append(frames, cf...)
		s.msgItem = nil
	}
	// Close open reasoning item.
	if s.reasoningItem != nil {
		cf, err := s.closeReasoningItem(s.reasoningItem)
		if err != nil {
			return nil, err
		}
		frames = append(frames, cf...)
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
		ti = &trackedItem{
			kind:        kindToolCall,
			outputIndex: s.nextIndex,
			itemID:      itemID,
			callID:      tc.ID,
			name:        name,
		}
		s.nextIndex++
		s.toolItems[tc.Index] = ti

		fcItem := &responses.FunctionCall{
			ID:     ti.itemID,
			CallID: ti.callID,
			Name:   name,
			Status: responses.StatusInProgress,
		}
		addedPayload, err := json.Marshal(&responses.ItemAddedEvent{
			OutputIndex: ti.outputIndex,
			Item:        fcItem,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, SSEFrame{Event: responses.EventOutputItemAdded, Data: addedPayload})
	}

	// Accumulate and emit args delta.
	if tc.Function != nil && tc.Function.Arguments != "" {
		ti.argsBuf += tc.Function.Arguments
		deltaPayload, err := json.Marshal(&responses.FunctionCallArgumentsDeltaEvent{
			ItemID:      ti.itemID,
			OutputIndex: ti.outputIndex,
			CallID:      ti.callID,
			Delta:       tc.Function.Arguments,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, SSEFrame{Event: responses.EventFunctionCallArgumentsDelta, Data: deltaPayload})
	}

	return frames, nil
}

// closeMsgItem emits output_text.done → content_part.done → output_item.done.
func (s *Stream) closeMsgItem(ti *trackedItem) ([]SSEFrame, error) {
	var frames []SSEFrame

	textDone, err := json.Marshal(&responses.OutputTextDoneEvent{
		ItemID:       ti.itemID,
		OutputIndex:  ti.outputIndex,
		ContentIndex: 0,
		Text:         ti.textBuf,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventOutputTextDone, Data: textDone})

	finalPart := &responses.OutputTextPart{Text: ti.textBuf}
	partDone, err := json.Marshal(&responses.ContentPartDoneEvent{
		ItemID:       ti.itemID,
		OutputIndex:  ti.outputIndex,
		ContentIndex: 0,
		Part:         finalPart,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventContentPartDone, Data: partDone})

	finalMsg := &responses.Message{
		ID:      ti.itemID,
		Role:    responses.RoleAssistant,
		Status:  responses.StatusCompleted,
		Content: []responses.Part{finalPart},
	}
	itemDone, err := json.Marshal(&responses.OutputItemDoneEvent{
		OutputIndex: ti.outputIndex,
		Item:        finalMsg,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventOutputItemDone, Data: itemDone})

	s.closedItems = append(s.closedItems, finalMsg)
	return frames, nil
}

// closeReasoningItem emits reasoning_text.done → output_item.done.
func (s *Stream) closeReasoningItem(ti *trackedItem) ([]SSEFrame, error) {
	var frames []SSEFrame

	textDone, err := json.Marshal(&responses.ReasoningTextDoneEvent{
		ItemID:       ti.itemID,
		OutputIndex:  ti.outputIndex,
		ContentIndex: 0,
		Text:         ti.textBuf,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventReasoningTextDone, Data: textDone})

	finalItem := &responses.Reasoning{
		ID:      ti.itemID,
		Summary: []responses.SummaryText{{Text: ti.textBuf}},
		Status:  responses.StatusCompleted,
	}
	itemDone, err := json.Marshal(&responses.OutputItemDoneEvent{
		OutputIndex: ti.outputIndex,
		Item:        finalItem,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventOutputItemDone, Data: itemDone})

	s.closedItems = append(s.closedItems, finalItem)
	return frames, nil
}

// closeToolItem emits function_call_arguments.done → output_item.done.
func (s *Stream) closeToolItem(ti *trackedItem) ([]SSEFrame, error) {
	var frames []SSEFrame

	argsDone, err := json.Marshal(&responses.FunctionCallArgumentsDoneEvent{
		ItemID:      ti.itemID,
		OutputIndex: ti.outputIndex,
		CallID:      ti.callID,
		Arguments:   ti.argsBuf,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventFunctionCallArgumentsDone, Data: argsDone})

	finalItem := &responses.FunctionCall{
		ID:        ti.itemID,
		CallID:    ti.callID,
		Name:      ti.name,
		Arguments: ti.argsBuf,
		Status:    responses.StatusCompleted,
	}

	itemDone, err := json.Marshal(&responses.OutputItemDoneEvent{
		OutputIndex: ti.outputIndex,
		Item:        finalItem,
	})
	if err != nil {
		return nil, err
	}
	frames = append(frames, SSEFrame{Event: responses.EventOutputItemDone, Data: itemDone})

	s.closedItems = append(s.closedItems, finalItem)
	return frames, nil
}

// buildFinalResponse constructs the final Response for the completed event.
func (s *Stream) buildFinalResponse() *responses.Response {
	resp := &responses.Response{
		ID:        s.responseID,
		Object:    "response",
		CreatedAt: s.created,
		Model:     s.model,
		Status:    responses.StatusCompleted,
		Output:    append([]responses.Item{}, s.closedItems...),
	}

	if s.lastUsage != nil {
		resp.Usage = translateUsage(s.lastUsage)
	}

	return resp
}

// extractReasoningContent looks for a non-standard reasoning_content field in
// the raw stream chunk delta. Some o-series upstreams emit it there.
func extractReasoningContent(raw []byte) string {
	var probe struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	if len(probe.Choices) == 0 {
		return ""
	}
	return probe.Choices[0].Delta.ReasoningContent
}

// parseSSEChunk extracts the data payload from a raw SSE chunk.
// Mirrors the implementation in pkg/adapters/anthropic for consistency.
func parseSSEChunk(chunk []byte) (event string, data []byte, ok bool) {
	lines := bytes.Split(bytes.TrimRight(chunk, "\n"), []byte("\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("event:")) {
			event = string(bytes.TrimSpace(line[6:]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(line[5:])
		}
	}
	return event, data, len(data) > 0
}

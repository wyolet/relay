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

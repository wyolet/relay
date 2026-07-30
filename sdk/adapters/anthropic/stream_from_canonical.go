package anthropic

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- NewFromCanonicalStream ----

// NewFromCanonicalStream returns a stateful per-stream function that converts
// canonical SSE chunks into Anthropic SSE chunks.
func (AnthropicTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToAnthropicStream{}
	return s.translate
}

// ---- stream: canonical → Anthropic ----

type canonicalToAnthropicStream struct {
	responseID            string
	model                 string
	blockIndex            int
	blockIndexByCanonical map[int]int
	startEmitted          bool
}

func (s *canonicalToAnthropicStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	switch event {
	case v1.EventGenerationCreated:
		return s.handleGenerationCreated(data)
	case v1.EventItemStarted:
		return s.handleItemStarted(data)
	case v1.EventItemDelta:
		return s.handleItemDelta(data)
	case v1.EventItemCompleted:
		return s.handleItemCompleted(data)
	case v1.EventGenerationCompleted:
		return s.handleGenerationCompleted(data)
	case v1.EventError:
		return s.handleCanonError(data)
	default:
		return nil, nil
	}
}

func (s *canonicalToAnthropicStream) handleGenerationCreated(data []byte) ([]byte, error) {
	var e v1.GenerationCreatedEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: generation.created: %w", err)
	}
	s.responseID = e.ID
	s.model = e.Model

	// Emit message_start + ping
	ms, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.responseID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
	ping, _ := json.Marshal(map[string]string{"type": "ping"})

	var out []byte
	out = append(out, anthropicSSEBytes("message_start", string(ms))...)
	out = append(out, anthropicSSEBytes("ping", string(ping))...)
	return out, nil
}

func (s *canonicalToAnthropicStream) handleItemStarted(data []byte) ([]byte, error) {
	var e v1.ItemStartedEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: item.started: %w", err)
	}

	idx := s.blockIndex
	s.blockIndex++

	// Each Anthropic content block type requires different fields on the
	// opening frame. Emitting the wrong shape (e.g. text:"" on tool_use)
	// trips strict clients like Claude Code with `undefined.slice()` when
	// they read name/text from the block before any delta arrives.
	var cb map[string]any
	switch e.ItemType {
	case v1.ItemTypeMessage:
		cb = map[string]any{"type": "text", "text": ""}
	case v1.ItemTypeFunctionCall:
		cb = map[string]any{
			"type":  "tool_use",
			"id":    e.ItemID,
			"name":  e.Name,
			"input": map[string]any{},
		}
	case v1.ItemTypeReasoning:
		cb = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	default:
		return nil, nil
	}
	if s.blockIndexByCanonical == nil {
		s.blockIndexByCanonical = make(map[int]int)
	}
	s.blockIndexByCanonical[e.Index] = idx

	cbs, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": cb,
	})
	return anthropicSSEBytes("content_block_start", string(cbs)), nil
}

func (s *canonicalToAnthropicStream) handleItemDelta(data []byte) ([]byte, error) {
	var e v1.ItemDeltaEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: item.delta: %w", err)
	}

	idx, ok := s.blockIndexByCanonical[e.Index]
	if !ok {
		return nil, nil
	}
	var deltaType string
	var deltaKey string
	switch e.Kind {
	case v1.DeltaKindText:
		deltaType = "text_delta"
		deltaKey = "text"
	case v1.DeltaKindArguments:
		deltaType = "input_json_delta"
		deltaKey = "partial_json"
	case v1.DeltaKindReasoning:
		deltaType = "thinking_delta"
		deltaKey = "thinking"
	default:
		return nil, nil
	}

	cbd, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]string{
			"type":   deltaType,
			deltaKey: e.Delta,
		},
	})
	return anthropicSSEBytes("content_block_delta", string(cbd)), nil
}

func (s *canonicalToAnthropicStream) handleItemCompleted(data []byte) ([]byte, error) {
	// Only need the index field; Item is polymorphic and not needed here.
	var e struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: item.completed: %w", err)
	}
	idx, ok := s.blockIndexByCanonical[e.Index]
	if !ok {
		return nil, nil
	}

	cbe, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
	return anthropicSSEBytes("content_block_stop", string(cbe)), nil
}

func (s *canonicalToAnthropicStream) handleGenerationCompleted(data []byte) ([]byte, error) {
	var e v1.GenerationCompletedEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: generation.completed: %w", err)
	}

	stopReason := canonicalFinishReasonToAnthropicStr(e.FinishReason)
	if e.Status == v1.StatusIncomplete && e.FinishReason == "" {
		stopReason = "pause_turn"
	}

	outTokens := int64(0)
	if len(e.Usage) > 0 {
		outTokens = e.Usage["output"]
	}

	md, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]string{
			"stop_reason":   stopReason,
			"stop_sequence": "",
		},
		"usage": map[string]int64{"output_tokens": outTokens},
	})
	ms, _ := json.Marshal(map[string]string{"type": "message_stop"})

	var out []byte
	out = append(out, anthropicSSEBytes("message_delta", string(md))...)
	out = append(out, anthropicSSEBytes("message_stop", string(ms))...)
	return out, nil
}

func (s *canonicalToAnthropicStream) handleCanonError(data []byte) ([]byte, error) {
	var e v1.ErrorEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: error: %w", err)
	}
	errB, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    e.Code,
			"message": e.Message,
		},
	})
	return anthropicSSEBytes("error", string(errB)), nil
}

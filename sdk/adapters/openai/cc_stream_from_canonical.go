package openai

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// NewFromCanonicalStream returns a stateful per-stream function that converts
// one canonical SSE chunk into one or more CC chat.completion.chunk SSE frames.
// This path is live whenever an inbound /v1/chat/completions caller is routed to
// a non-OpenAI upstream (e.g. Anthropic) — the pipeline composes
// anthropic.NewToCanonicalStream → this function.
func (CCTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToCCStream{}
	return s.translate
}

// --- helpers ---

// marshalCanonicalFrames serializes a slice of v1.SSEFrame values to wire bytes.
// Returns all frames concatenated.
// canonicalToCCStream converts canonical SSE frames to OpenAI chat.completion.chunk
// SSE frames. Used by NewFromCanonicalStream when a CC inbound caller is served by
// a non-CC upstream (e.g. Anthropic → canonical → CC).
type canonicalToCCStream struct {
	responseID string
	model      string
	created    int64
	toolItems  map[string]ccFromCanonicalToolItem // itemID → per-item state
}

type ccFromCanonicalToolItem struct {
	index  int // sequential index within this response's tool_calls array
	callID string
	name   string
}

func (s *canonicalToCCStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	if s.toolItems == nil {
		s.toolItems = make(map[string]ccFromCanonicalToolItem)
	}

	var out []byte

	switch event {
	case v1.EventGenerationCreated:
		var ev v1.GenerationCreatedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("cc from_canonical stream: created: %w", err)
		}
		s.responseID = ev.ID
		s.model = ev.Model
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		// Role-bearing first chunk; no content yet.
		role := "assistant"
		b, _ := json.Marshal(ChatStreamChunk{
			ID:      s.responseID,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []StreamChoice{{
				Index:        0,
				Delta:        StreamDelta{Role: role},
				FinishReason: nil,
			}},
		})
		out = append(out, ccSSEDataFrame(b)...)

	case v1.EventItemStarted:
		var ev v1.ItemStartedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		if ev.ItemType == v1.ItemTypeFunctionCall {
			idx := len(s.toolItems)
			// Emit the id+name header chunk for this tool call immediately:
			// CC streaming convention is id+type+name on the first chunk, then
			// arguments-only deltas follow.
			callID := ev.ItemID
			s.toolItems[ev.ItemID] = ccFromCanonicalToolItem{index: idx, callID: callID, name: ev.Name}
			b, _ := json.Marshal(ChatStreamChunk{
				ID:      s.responseID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: StreamDelta{
						ToolCalls: []ToolCallChunk{{
							Index: idx,
							ID:    callID,
							Type:  "function",
							Function: &ToolCallFunctionChunk{
								Name: ev.Name,
							},
						}},
					},
					FinishReason: nil,
				}},
			})
			out = append(out, ccSSEDataFrame(b)...)
		}

	case v1.EventItemDelta:
		var ev v1.ItemDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		switch ev.Kind {
		case v1.DeltaKindText:
			b, _ := json.Marshal(ChatStreamChunk{
				ID:      s.responseID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []StreamChoice{{
					Index:        0,
					Delta:        StreamDelta{Content: &ev.Delta},
					FinishReason: nil,
				}},
			})
			out = append(out, ccSSEDataFrame(b)...)

		case v1.DeltaKindArguments:
			ti, ok := s.toolItems[ev.ItemID]
			if !ok {
				break
			}
			b, _ := json.Marshal(ChatStreamChunk{
				ID:      s.responseID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []StreamChoice{{
					Index: 0,
					Delta: StreamDelta{
						ToolCalls: []ToolCallChunk{{
							Index:    ti.index,
							Function: &ToolCallFunctionChunk{Arguments: ev.Delta},
						}},
					},
					FinishReason: nil,
				}},
			})
			out = append(out, ccSSEDataFrame(b)...)

		case v1.DeltaKindReasoning:
			// Emits the "reasoning_content" field. Unlike the non-stream path,
			// the canonical Reasoning item's provider_data (which records the
			// original Ollama-vs-OpenAI field) only arrives at item.completed —
			// after these deltas have already flushed — so the wire field can't
			// be preserved here without buffering. reasoning_content is the safe
			// default; clients that don't understand it skip it.
			b, _ := ccMarshalReasoningChunk(s.responseID, s.model, s.created, ev.Delta)
			out = append(out, ccSSEDataFrame(b)...)
		}

	case v1.EventItemCompleted:
		// item.completed carries the full assembled item; use it to patch in the
		// real call_id for any tool call whose id we only know now.
		var evHeader struct {
			ItemID string `json:"item_id"`
			Item   struct {
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(data, &evHeader); err != nil {
			return nil, nil
		}
		if ti, ok := s.toolItems[evHeader.ItemID]; ok {
			if evHeader.Item.CallID != "" && ti.callID != evHeader.Item.CallID {
				ti.callID = evHeader.Item.CallID
				s.toolItems[evHeader.ItemID] = ti
			}
		}

	case v1.EventGenerationCompleted:
		var ev v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		fr := canonicalFinishReasonToCC(ev.FinishReason)
		finalChunk := ChatStreamChunk{
			ID:      s.responseID,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []StreamChoice{{
				Index:        0,
				Delta:        StreamDelta{},
				FinishReason: &fr,
			}},
		}
		if len(ev.Usage) > 0 {
			finalChunk.Usage = canonicalUsageToCC(ev.Usage)
		}
		b, _ := json.Marshal(finalChunk)
		out = append(out, ccSSEDataFrame(b)...)
		out = append(out, []byte("data: [DONE]\n\n")...)

	case v1.EventError:
		var ev v1.ErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		errBody := map[string]any{
			"error": map[string]string{
				"message": ev.Message,
				"type":    ev.Code,
				"code":    ev.Code,
			},
		}
		b, _ := json.Marshal(errBody)
		out = append(out, ccSSEDataFrame(b)...)
		out = append(out, []byte("data: [DONE]\n\n")...)
	}

	return out, nil
}

// ccSSEDataFrame wraps JSON bytes in a CC SSE data frame (no event: line — CC
// uses bare data: lines, not event: lines).
func ccSSEDataFrame(data []byte) []byte {
	frame := make([]byte, 0, len(data)+8)
	frame = append(frame, []byte("data: ")...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return frame
}

// canonicalFinishReasonToCC maps canonical FinishReason to CC finish_reason strings.
func canonicalFinishReasonToCC(fr v1.FinishReason) string {
	switch fr {
	case v1.FinishReasonStop:
		return "stop"
	case v1.FinishReasonLength:
		return "length"
	case v1.FinishReasonToolCalls:
		return "tool_calls"
	case v1.FinishReasonContentFilter:
		return "content_filter"
	case v1.FinishReasonRefusal:
		return "stop"
	default:
		return "stop"
	}
}

// ccMarshalReasoningChunk builds a CC chunk carrying a reasoning_content delta.
func ccMarshalReasoningChunk(id, model string, created int64, delta string) ([]byte, error) {
	type reasoningDelta struct {
		ReasoningContent string `json:"reasoning_content"`
	}
	type reasoningChoice struct {
		Index        int            `json:"index"`
		Delta        reasoningDelta `json:"delta"`
		FinishReason *string        `json:"finish_reason"`
	}
	type reasoningChunk struct {
		ID      string            `json:"id"`
		Object  string            `json:"object"`
		Created int64             `json:"created"`
		Model   string            `json:"model"`
		Choices []reasoningChoice `json:"choices"`
	}
	return json.Marshal(reasoningChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []reasoningChoice{{
			Index: 0,
			Delta: reasoningDelta{ReasoningContent: delta},
		}},
	})
}

func marshalCanonicalFrames(frames []v1.SSEFrame) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
}

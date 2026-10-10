package anthropic

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/sdk/internal/sse"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- NewToCanonicalStream ----

// NewToCanonicalStream returns a stateful per-stream function that converts
// Anthropic SSE chunks into canonical SSE chunks.
func (AnthropicTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &anthropicToCanonicalStream{}
	return s.translate
}

// ---- stream: Anthropic → canonical ----

type anthropicToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	nextIndex        int
	lifecycleEmitted bool
	currentBlock     *anthropicStreamBlock
	// accumulated usage from message_start + message_delta
	inputTokens         int
	outputTokens        int
	cachedTokens        int
	cacheCreationTokens int
	cacheCreation1h     int
	stopReason          string
	// structuredOutputSeen is set when a __relay_structured_output tool block
	// is completed. It prevents handleMessageDelta from overwriting the
	// already-corrected stop reason with "tool_use".
	structuredOutputSeen bool
}

func (s *anthropicToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	var out []byte
	sc := sse.NewScanner(chunk)
	for sc.Next() {
		b, err := s.translateEvent(sc.Event(), sc.Data())
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = b
		} else {
			out = append(out, b...)
		}
	}
	return out, nil
}

func (s *anthropicToCanonicalStream) translateEvent(event, data []byte) ([]byte, error) {
	switch string(event) {
	case "message_start":
		return s.handleMessageStart(data)
	case "content_block_start":
		return s.handleContentBlockStart(data)
	case "content_block_delta":
		return s.handleContentBlockDelta(data)
	case "content_block_stop":
		return s.handleContentBlockStop(data)
	case "message_delta":
		return s.handleMessageDelta(data)
	case "message_stop":
		return s.handleMessageStop()
	case "error":
		return s.handleError(data)
	case "ping", "":
		return nil, nil
	default:
		// canonical: unrecognized (future) stream events dropped — the six
		// canonical events are a closed union with no slot to carry them, and
		// message_delta/message_stop carry the terminal outcome explicitly above.
		return nil, nil
	}
}

func (s *anthropicToCanonicalStream) handleMessageStart(data []byte) ([]byte, error) {
	var ms struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens        int `json:"input_tokens"`
				CacheRead          int `json:"cache_read_input_tokens"`
				CacheCreation      int `json:"cache_creation_input_tokens"`
				CacheCreationByTTL struct {
					Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("anthropic stream: message_start: %w", err)
	}
	s.responseID = ms.Message.ID
	s.model = ms.Message.Model
	s.created = time.Now().Unix()
	s.inputTokens = ms.Message.Usage.InputTokens
	s.cachedTokens = ms.Message.Usage.CacheRead
	s.cacheCreationTokens = ms.Message.Usage.CacheCreation
	s.cacheCreation1h = ms.Message.Usage.CacheCreationByTTL.Ephemeral1h

	if s.responseID == "" {
		s.responseID = fmt.Sprintf("resp_%d", s.created)
	}

	createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
		ID:    s.responseID,
		Model: s.model,
	})
	s.lifecycleEmitted = true
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCreated, Data: createdData}}), nil
}

func (s *anthropicToCanonicalStream) handleMessageDelta(data []byte) ([]byte, error) {
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens       int `json:"output_tokens"`
			InputTokens        int `json:"input_tokens"`
			CacheRead          int `json:"cache_read_input_tokens"`
			CacheCreation      int `json:"cache_creation_input_tokens"`
			CacheCreationByTTL struct {
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("anthropic stream: message_delta: %w", err)
	}
	// Don't let the wire "tool_use" stop_reason clobber the "end_turn" we
	// already committed when the structured-output tool block completed.
	if !s.structuredOutputSeen {
		s.stopReason = md.Delta.StopReason
	}
	s.outputTokens = md.Usage.OutputTokens
	// message_delta usage is cumulative and its input-side counts are optional; max keeps message_start's when they are absent or zero.
	s.inputTokens = max(s.inputTokens, md.Usage.InputTokens)
	s.cachedTokens = max(s.cachedTokens, md.Usage.CacheRead)
	s.cacheCreationTokens = max(s.cacheCreationTokens, md.Usage.CacheCreation)
	s.cacheCreation1h = max(s.cacheCreation1h, md.Usage.CacheCreationByTTL.Ephemeral1h)
	return nil, nil
}

func (s *anthropicToCanonicalStream) handleMessageStop() ([]byte, error) {
	status, finish, incomplete := anthropicStopReasonToCanonical(s.stopReason)

	gen := v1.GenerationCompletedEvent{
		ID:                s.responseID,
		Status:            status,
		FinishReason:      finish,
		Usage:             anthropicTokens(s.inputTokens, s.outputTokens, s.cachedTokens, s.cacheCreationTokens, s.cacheCreation1h),
		IncompleteDetails: incomplete,
	}

	completedData, _ := json.Marshal(gen)
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCompleted, Data: completedData}}), nil
}

func (s *anthropicToCanonicalStream) handleError(data []byte) ([]byte, error) {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = string(data)
	}
	errData, _ := json.Marshal(v1.ErrorEvent{
		Code:    e.Error.Type,
		Message: msg,
	})
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventError, Data: errData}}), nil
}

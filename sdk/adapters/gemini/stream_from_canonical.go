package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- NewFromCanonicalStream ----

// NewFromCanonicalStream returns a stateful per-stream function that converts
// canonical SSE chunks into Gemini SSE frames.
func (GeminiTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToGeminiStream{}
	return s.translate
}

// ---- stream: canonical → Gemini ----

type canonicalToGeminiStream struct {
	responseID string
	model      string
	// accumulated parts for the current candidate
	parts        []geminiPart
	finishReason string
	// current function-call item being assembled. Gemini does not stream
	// partial function args (unlike canonical's arguments deltas), so we
	// buffer them and emit one complete functionCall frame on item.completed.
	inFunctionCall bool
	fcName         string
	fcArgs         strings.Builder
}

func (s *canonicalToGeminiStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	switch event {
	case v1.EventGenerationCreated:
		var e v1.GenerationCreatedEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: generation.created: %w", err)
		}
		s.responseID = e.ID
		s.model = e.Model
		return nil, nil // Gemini has no session-open frame

	case v1.EventItemStarted:
		var e v1.ItemStartedEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: item.started: %w", err)
		}
		if e.ItemType == v1.ItemTypeFunctionCall {
			// Begin buffering a function call; args arrive as deltas and the
			// complete functionCall is emitted on item.completed.
			s.inFunctionCall = true
			s.fcName = e.Name
			s.fcArgs.Reset()
		}
		return nil, nil

	case v1.EventItemDelta:
		var e v1.ItemDeltaEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: item.delta: %w", err)
		}
		// Text and reasoning stream incrementally (Gemini streams text parts).
		// Arguments are buffered, not streamed — Gemini emits functionCall
		// whole, so accumulate and flush on item.completed.
		var p geminiPart
		switch e.Kind {
		case v1.DeltaKindText:
			p = geminiPart{Text: e.Delta}
		case v1.DeltaKindReasoning:
			p = geminiPart{Text: e.Delta, Thought: true}
		case v1.DeltaKindArguments:
			s.fcArgs.WriteString(e.Delta)
			return nil, nil
		default:
			return nil, nil
		}
		frame := geminiResponse{
			Candidates: []candidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{p}},
				Index:   0,
			}},
			ModelVersion: s.model,
		}
		return geminiSSEBytes(frame)

	case v1.EventItemCompleted:
		if !s.inFunctionCall {
			// Text/reasoning content already streamed via deltas.
			return nil, nil
		}
		// Flush the buffered function call as one complete frame. The
		// accumulated args are a complete JSON object string; embed verbatim
		// (fall back to {} if empty or invalid).
		s.inFunctionCall = false
		args := json.RawMessage("{}")
		if a := s.fcArgs.String(); a != "" && json.Valid([]byte(a)) {
			args = json.RawMessage(a)
		}
		frame := geminiResponse{
			Candidates: []candidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{{
					FunctionCall: &geminiFC{Name: s.fcName, Args: args},
				}}},
				Index: 0,
			}},
			ModelVersion: s.model,
		}
		return geminiSSEBytes(frame)

	case v1.EventGenerationCompleted:
		var e v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: generation.completed: %w", err)
		}
		finReason := canonicalFinishReasonToGemini(e.FinishReason, nil)
		frame := map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{}},
				"finishReason": finReason,
				"index":        0,
			}},
			"modelVersion": s.model,
		}
		if len(e.Usage) > 0 {
			um := map[string]int64{}
			if v := e.Usage["input"]; v > 0 {
				um["promptTokenCount"] = v
			}
			if v := e.Usage["output"]; v > 0 {
				um["candidatesTokenCount"] = v
			}
			if v := e.Usage["cache_read"]; v > 0 {
				um["cachedContentTokenCount"] = v
			}
			if v := e.Usage["reasoning"]; v > 0 {
				um["thoughtsTokenCount"] = v
			}
			frame["usageMetadata"] = um
		}
		b, err := json.Marshal(frame)
		if err != nil {
			return nil, err
		}
		return geminiSSEBytesRaw(b), nil

	case v1.EventError:
		var e v1.ErrorEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: error: %w", err)
		}
		errFrame := map[string]any{
			"error": map[string]any{
				"code":    500,
				"message": e.Message,
				"status":  e.Code,
			},
		}
		b, _ := json.Marshal(errFrame)
		return geminiSSEBytesRaw(b), nil

	default:
		return nil, nil
	}
}

package openai

import (
	"bytes"
)

// ResponsesTranslator implements v1.Translator for the OpenAI Responses API wire shape.
// The Responses wire shape is closely aligned with canonical — it is essentially
// "canonical with OpenAI-isms layered on top" (previous_response_id, store, conversation,
// background, etc.). Translation is near-identity for the core fields.
//
// Stateful OpenAI-isms (previous_response_id, store, conversation, background,
// include[], context_management, service_tier, safety_identifier)
// are rejected at ParseRequest with an explicit error so the caller can map to 400.
// These fields have no canonical equivalent in v1. prompt_cache_key and
// prompt_cache_retention are NOT among them: they map to the canonical cache
// intent (v1.CacheConfig.Key / .TTL) both ways.
//
// Request-echo fields required by the OpenAI spec (instructions, temperature, top_p,
// tools, tool_choice, parallel_tool_calls, metadata) are passed explicitly through
// SerializeResponse's req parameter.
type ResponsesTranslator struct{}

// composedStream chains a toCanonical function and a fromCanonical function:
// input chunk → toCanonical → canonical chunks → fromCanonical → output chunks.
// Both functions return []byte (concatenated SSE frames); we split them and
// feed each canonical frame into fromCanonical.
//
// Used to compose CCTranslator.NewToCanonicalStream with
// ResponsesTranslator.NewFromCanonicalStream.
type ComposedStream struct {
	toCanonical   func([]byte) ([]byte, error)
	fromCanonical func([]byte) ([]byte, error)
}

// NewComposedStream creates a ComposedStream from two translator stream functions.
func NewComposedStream(toCanonical, fromCanonical func([]byte) ([]byte, error)) *ComposedStream {
	return &ComposedStream{toCanonical: toCanonical, fromCanonical: fromCanonical}
}

// Translate processes one upstream chunk through the canonical chain and returns
// the translated output frames.
func (c *ComposedStream) Translate(chunk []byte) ([]ResponsesSSEFrame, error) {
	canonBytes, err := c.toCanonical(chunk)
	if err != nil {
		return nil, err
	}
	if len(canonBytes) == 0 {
		return nil, nil
	}

	// Split canonical bytes into individual SSE frames and feed each to fromCanonical.
	var out []ResponsesSSEFrame
	frames := splitSSEFrames(canonBytes)
	for _, frame := range frames {
		outBytes, err := c.fromCanonical(frame)
		if err != nil {
			return nil, err
		}
		// Parse the output bytes back into ResponsesSSEFrames.
		responseFrames := splitResponsesSSEFrames(outBytes)
		out = append(out, responseFrames...)
	}
	return out, nil
}

// splitSSEFrames splits concatenated SSE wire bytes into individual \n\n-delimited frames.
func splitSSEFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		idx := bytes.Index(b, []byte("\n\n"))
		if idx < 0 {
			if len(bytes.TrimSpace(b)) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		frame := b[:idx+2]
		if len(bytes.TrimSpace(b[:idx])) > 0 {
			frames = append(frames, frame)
		}
		b = b[idx+2:]
	}
	return frames
}

// splitResponsesSSEFrames parses concatenated ResponsesSSEFrame wire bytes back into structs.
func splitResponsesSSEFrames(b []byte) []ResponsesSSEFrame {
	var frames []ResponsesSSEFrame
	for _, raw := range splitSSEFrames(b) {
		event, data, ok := ParseResponsesSSEChunk(raw)
		if !ok {
			continue
		}
		frames = append(frames, ResponsesSSEFrame{Event: event, Data: data})
	}
	return frames
}

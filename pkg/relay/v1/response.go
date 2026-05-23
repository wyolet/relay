package v1

import (
	"encoding/json"

	"github.com/wyolet/relay/pkg/usage"
)

// Response is the canonical non-streaming response.
// It carries only what was produced — no request-echo fields.
// Vendor adapters that require echo (e.g. OpenAI Responses) receive the
// original *Request explicitly via SerializeResponse.
type Response struct {
	ID           string       `json:"id"`
	Object       string       `json:"object"`              // always "response"
	CreatedAt    int64        `json:"created_at"`          // unix seconds
	Model        string       `json:"model"`
	Status       Status       `json:"status"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Output       []Item       `json:"output,omitempty"`

	// Usage is the orthogonal-meter token map (input, output, cache_read,
	// cache_creation, reasoning, audio_input, audio_output, …). Keys
	// match pricing.MeterForUsageKey so the same vocabulary flows from
	// adapters through observers through pricing without translation.
	// Tokens.Sum() returns the honest "all tokens processed" total —
	// dimensions are non-overlapping by construction.
	Usage usage.Tokens `json:"usage,omitempty"`

	Error             *Error             `json:"error,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`

	// Extensions carries vendor-specific or cross-cutting response fields.
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// Error is an API-level error embedded in a response object.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IncompleteDetails explains why a response was not completed.
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

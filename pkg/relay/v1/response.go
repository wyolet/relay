package v1

import (
	"encoding/json"
	"fmt"

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

// ParseResponse decodes a canonical response body — the inverse of marshaling
// a *Response. It exists because Output is an interface slice ([]Item) whose
// concrete types are chosen by each item's "type" field, so the stdlib decoder
// can't reconstruct them on its own. Used by canonical clients reading
// /v1/generate responses.
func ParseResponse(body []byte) (*Response, error) {
	var raw struct {
		ID                string             `json:"id"`
		Object            string             `json:"object"`
		CreatedAt         int64              `json:"created_at"`
		Model             string             `json:"model"`
		Status            Status             `json:"status"`
		FinishReason      FinishReason       `json:"finish_reason"`
		Output            json.RawMessage    `json:"output"`
		Usage             usage.Tokens       `json:"usage"`
		Error             *Error             `json:"error"`
		IncompleteDetails *IncompleteDetails `json:"incomplete_details"`
		Extensions        map[string]json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	resp := &Response{
		ID:                raw.ID,
		Object:            raw.Object,
		CreatedAt:         raw.CreatedAt,
		Model:             raw.Model,
		Status:            raw.Status,
		FinishReason:      raw.FinishReason,
		Usage:             raw.Usage,
		Error:             raw.Error,
		IncompleteDetails: raw.IncompleteDetails,
		Extensions:        raw.Extensions,
	}
	if len(raw.Output) > 0 && string(raw.Output) != "null" {
		items, err := unmarshalItems(raw.Output)
		if err != nil {
			return nil, fmt.Errorf("output: %w", err)
		}
		resp.Output = items
	}
	return resp, nil
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

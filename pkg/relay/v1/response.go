package v1

import "encoding/json"

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
	Usage        *Usage       `json:"usage,omitempty"`

	Error             *Error             `json:"error,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`

	// Extensions carries vendor-specific or cross-cutting response fields.
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// Usage carries token counts for the response.
type Usage struct {
	InputTokens         int         `json:"input_tokens"`
	OutputTokens        int         `json:"output_tokens"`
	TotalTokens         int         `json:"total_tokens"`
	InputTokensDetails  InputDeets  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails OutputDeets `json:"output_tokens_details,omitempty"`
}

// InputDeets holds per-category input token counts.
type InputDeets struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// OutputDeets holds per-category output token counts.
type OutputDeets struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
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

package responses

// Response is the non-streaming response object from POST /v1/responses.
// It holds only response-side fields; request fields are not echoed.
type Response struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`          // always "response"
	CreatedAt         int64              `json:"created_at"`      // unix seconds
	Model             string             `json:"model"`
	Status            Status             `json:"status"`
	FinishReason      FinishReason       `json:"finish_reason,omitempty"`
	Output            []Item             `json:"output"`
	Usage             *Usage             `json:"usage,omitempty"`
	Error             *Error             `json:"error,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
}

// Usage carries token counts for the response.
type Usage struct {
	InputTokens         int          `json:"input_tokens"`
	OutputTokens        int          `json:"output_tokens"`
	TotalTokens         int          `json:"total_tokens"`
	InputTokensDetails  *InputDeets  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputDeets `json:"output_tokens_details,omitempty"`
}

// InputDeets holds per-category input token counts.
type InputDeets struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputDeets holds per-category output token counts.
type OutputDeets struct {
	ReasoningTokens int `json:"reasoning_tokens"`
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

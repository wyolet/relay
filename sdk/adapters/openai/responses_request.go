package openai

import "encoding/json"

// ResponsesRequest is the parsed body for POST /v1/responses.
// Input is always normalized to []ResponsesItem internally; callers never see the
// string form.
type ResponsesRequest struct {
	Model string          `json:"model"`
	Input []ResponsesItem `json:"-"` // normalized; string form is expanded at parse

	Instructions string               `json:"instructions,omitempty"`
	Tools        ResponsesTools       `json:"tools,omitempty"`
	ToolChoice   *ResponsesToolChoice `json:"tool_choice,omitempty"`

	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`

	Text      *ResponsesTextConfig      `json:"text,omitempty"`
	Reasoning *ResponsesReasoningConfig `json:"reasoning,omitempty"`

	MaxToolCalls *int            `json:"max_tool_calls,omitempty"`
	Prompt       json.RawMessage `json:"prompt,omitempty"` // stored prompt template ref; stateful

	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	User              string            `json:"user,omitempty"`
	Stream            *bool             `json:"stream,omitempty"`
	StopSequences     []string          `json:"stop_sequences,omitempty"`

	// Forward-compat fields: parsed but left opaque for Phase 2.
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Conversation       string          `json:"conversation,omitempty"`
	Background         *bool           `json:"background,omitempty"`
	Truncation         string          `json:"truncation,omitempty"`
	ServiceTier        string          `json:"service_tier,omitempty"`
	SafetyIdentifier   string          `json:"safety_identifier,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	Logprobs           *bool           `json:"logprobs,omitempty"`
	TopLogprobs        *int            `json:"top_logprobs,omitempty"`
	Include            []string        `json:"include,omitempty"`
	ContextManagement  json.RawMessage `json:"context_management,omitempty"`
}

// ResponsesTextConfig controls the output text format and verbosity.
type ResponsesTextConfig struct {
	Format    *ResponsesFormat `json:"format,omitempty"`
	Verbosity string           `json:"verbosity,omitempty"` // "low" | "medium" | "high"
}

// ResponsesFormat specifies the response format type.
// Type is one of: "text", "json_object", "json_schema".
type ResponsesFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`        // json_schema only
	Description string          `json:"description,omitempty"` // json_schema only
	Schema      json.RawMessage `json:"schema,omitempty"`      // json_schema only
	Strict      *bool           `json:"strict,omitempty"`      // json_schema only
}

// ResponsesReasoningConfig controls reasoning effort.
// Effort is one of: "none", "minimal", "low", "medium", "high", "xhigh".
// Summary is one of: "auto", "concise", "detailed".
type ResponsesReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

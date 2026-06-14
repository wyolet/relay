package v1

import "encoding/json"

// ModelOpts is per-model configuration keyed by model name in
// Request.ModelConfig. All fields are optional; nil means use the vendor
// default. Tool definitions are NOT here — they are task-level and live in
// Request.Tools (one spec shared across all models).
type ModelOpts struct {
	Sampling  *SamplingParams  `json:"sampling,omitempty"`
	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`
	Output    *OutputConfig    `json:"output,omitempty"`
}

// SamplingParams controls token sampling. All fields default-OK when nil.
type SamplingParams struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	MaxTokens        *int     `json:"max_tokens,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
}

// ReasoningConfig controls reasoning effort.
// Effort: "none" | "minimal" | "low" | "medium" | "high" | "xhigh".
// Summary: "auto" | "concise" | "detailed".
type ReasoningConfig struct {
	Effort       string `json:"effort,omitempty"`
	Summary      string `json:"summary,omitempty"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}

// OutputConfig controls output format and verbosity.
type OutputConfig struct {
	Format    *Format `json:"format,omitempty"`
	Verbosity string  `json:"verbosity,omitempty"` // "low" | "medium" | "high"
}

// Format specifies the response format type.
// Type is one of: "text", "json_object", "json_schema".
type Format struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`        // json_schema only
	Description string          `json:"description,omitempty"` // json_schema only
	Schema      json.RawMessage `json:"schema,omitempty"`      // json_schema only
	Strict      *bool           `json:"strict,omitempty"`      // json_schema only
}

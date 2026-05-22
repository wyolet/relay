package v1

import "encoding/json"

// Request is the canonical inbound request body for /v1/generate.
// Input is always normalized to []Item internally; callers never see the
// string form.
type Request struct {
	Model  string `json:"model"`
	Input  []Item `json:"-"` // normalized; string form expanded at parse

	Instructions    string      `json:"instructions,omitempty"`
	Tools           Tools       `json:"tools,omitempty"`
	ToolChoice      *ToolChoice `json:"tool_choice,omitempty"`

	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`

	Text      *TextConfig      `json:"text,omitempty"`
	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`

	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	User              string   `json:"user,omitempty"`
	Stream            *bool    `json:"stream,omitempty"`
	StopSequences     []string `json:"stop_sequences,omitempty"`

	// Extensions carries vendor-specific or cross-cutting fields that don't
	// map cleanly across all vendors (cache hints, safety settings, etc.).
	// Vendor adapters that understand a key emit the corresponding wire field;
	// adapters that don't, ignore it.
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// TextConfig controls the output text format.
type TextConfig struct {
	Format *Format `json:"format,omitempty"`
}

// Format specifies the response format type.
// Type is one of: "text", "json_object", "json_schema".
type Format struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`   // json_schema only
	Schema json.RawMessage `json:"schema,omitempty"` // json_schema only
	Strict *bool           `json:"strict,omitempty"` // json_schema only
}

// ReasoningConfig controls reasoning effort.
// Effort is one of: "none", "minimal", "low", "medium", "high", "xhigh".
type ReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

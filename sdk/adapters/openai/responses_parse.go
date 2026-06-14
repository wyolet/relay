package openai

import (
	"encoding/json"
	"fmt"
)

// ParseResponsesRequest decodes a POST /v1/responses body into a *ResponsesRequest.
//
// Normalization applied:
//   - string input → []ResponsesItem{&ResponsesMessage{Role:ResponsesRoleUser, Content:[]ResponsesPart{&ResponsesTextPart{...}}}}
//   - string content inside messages → []ResponsesPart{&ResponsesTextPart{...}}
//   - unsupported item types return an explicit error (caller maps to 400)
//   - unsupported tool types return an explicit error (caller maps to 400)
//   - metadata exceeding caps returns an explicit error (caller maps to 400)
func ParseResponsesRequest(body []byte) (*ResponsesRequest, error) {
	var wire struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`

		Instructions string          `json:"instructions"`
		Tools        json.RawMessage `json:"tools"`
		ToolChoice   json.RawMessage `json:"tool_choice"`

		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"top_p"`
		TopK            *int     `json:"top_k"`
		MaxOutputTokens *int     `json:"max_output_tokens"`

		Text      *ResponsesTextConfig      `json:"text"`
		Reasoning *ResponsesReasoningConfig `json:"reasoning"`

		MaxToolCalls *int            `json:"max_tool_calls"`
		Prompt       json.RawMessage `json:"prompt"`

		ParallelToolCalls *bool             `json:"parallel_tool_calls"`
		Metadata          map[string]string `json:"metadata"`
		User              string            `json:"user"`
		Stream            *bool             `json:"stream"`
		StopSequences     []string          `json:"stop_sequences"`

		PreviousResponseID string          `json:"previous_response_id"`
		Store              *bool           `json:"store"`
		Conversation       string          `json:"conversation"`
		Background         *bool           `json:"background"`
		Truncation         string          `json:"truncation"`
		ServiceTier        string          `json:"service_tier"`
		SafetyIdentifier   string          `json:"safety_identifier"`
		PromptCacheKey     string          `json:"prompt_cache_key"`
		Logprobs           *bool           `json:"logprobs"`
		TopLogprobs        *int            `json:"top_logprobs"`
		Include            []string        `json:"include"`
		ContextManagement  json.RawMessage `json:"context_management"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if wire.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(wire.Input) == 0 {
		return nil, fmt.Errorf("input is required")
	}

	// Normalize input.
	input, err := responsesNormalizeInput(wire.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	// Parse tools.
	var tools ResponsesTools
	if len(wire.Tools) > 0 && string(wire.Tools) != "null" {
		if err := json.Unmarshal(wire.Tools, &tools); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
	}

	// Parse tool_choice.
	var toolChoice *ResponsesToolChoice
	if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
		var tc ResponsesToolChoice
		if err := json.Unmarshal(wire.ToolChoice, &tc); err != nil {
			return nil, fmt.Errorf("tool_choice: %w", err)
		}
		toolChoice = &tc
	}

	// Validate metadata.
	if len(wire.Metadata) > 0 {
		if err := responsesValidateMetadata(wire.Metadata); err != nil {
			return nil, fmt.Errorf("metadata: %w", err)
		}
	}

	return &ResponsesRequest{
		Model:        wire.Model,
		Input:        input,
		Instructions: wire.Instructions,
		Tools:        tools,
		ToolChoice:   toolChoice,

		Temperature:     wire.Temperature,
		TopP:            wire.TopP,
		TopK:            wire.TopK,
		MaxOutputTokens: wire.MaxOutputTokens,

		Text:      wire.Text,
		Reasoning: wire.Reasoning,

		MaxToolCalls: wire.MaxToolCalls,
		Prompt:       wire.Prompt,

		ParallelToolCalls: wire.ParallelToolCalls,
		Metadata:          wire.Metadata,
		User:              wire.User,
		Stream:            wire.Stream,
		StopSequences:     wire.StopSequences,

		PreviousResponseID: wire.PreviousResponseID,
		Store:              wire.Store,
		Conversation:       wire.Conversation,
		Background:         wire.Background,
		Truncation:         wire.Truncation,
		ServiceTier:        wire.ServiceTier,
		SafetyIdentifier:   wire.SafetyIdentifier,
		PromptCacheKey:     wire.PromptCacheKey,
		Logprobs:           wire.Logprobs,
		TopLogprobs:        wire.TopLogprobs,
		Include:            wire.Include,
		ContextManagement:  wire.ContextManagement,
	}, nil
}

// responsesNormalizeInput converts the wire input (string or array) to []ResponsesItem.
// String "hello" → []ResponsesItem{&ResponsesMessage{Role:ResponsesRoleUser, Content:[]ResponsesPart{&ResponsesTextPart{Text:"hello"}}}}.
func responsesNormalizeInput(raw json.RawMessage) ([]ResponsesItem, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []ResponsesItem{&ResponsesMessage{
			Role:    ResponsesRoleUser,
			Content: []ResponsesPart{&ResponsesTextPart{Text: s}},
		}}, nil
	}
	return responsesUnmarshalItems(raw)
}

// responsesValidateMetadata enforces the caps on the metadata map:
// ≤16 entries, key ≤64 chars (DNS-1123 charset), value ≤256 chars (ASCII printable).
func responsesValidateMetadata(m map[string]string) error {
	if len(m) > 16 {
		return fmt.Errorf("too many entries: %d (max 16)", len(m))
	}
	for k, v := range m {
		if len(k) > 64 {
			return fmt.Errorf("key %q exceeds 64 characters", k)
		}
		if len(v) > 256 {
			return fmt.Errorf("value for key %q exceeds 256 characters", k)
		}
		if !responsesValidMetaKey(k) {
			return fmt.Errorf("key %q contains invalid characters (DNS-1123 charset required)", k)
		}
		if !responsesValidMetaValue(v) {
			return fmt.Errorf("value for key %q contains non-printable or disallowed ASCII characters", k)
		}
	}
	return nil
}

func responsesValidMetaKey(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func responsesValidMetaValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// ResponsesHints is the minimal set of fields extracted by ResponsesRoutingHints.
type ResponsesHints struct {
	Model              string
	Stream             bool
	User               string
	PreviousResponseID string
	Store              *bool
	Background         *bool
}

// ResponsesRoutingHints extracts the minimum fields needed for route-layer decisions
// without fully parsing the request body. Returns an error only for invalid JSON
// or missing model.
func ResponsesRoutingHints(body []byte) (*ResponsesHints, error) {
	var w struct {
		Model              string `json:"model"`
		Stream             *bool  `json:"stream"`
		User               string `json:"user"`
		PreviousResponseID string `json:"previous_response_id"`
		Store              *bool  `json:"store"`
		Background         *bool  `json:"background"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if w.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	h := &ResponsesHints{
		Model:              w.Model,
		User:               w.User,
		PreviousResponseID: w.PreviousResponseID,
		Store:              w.Store,
		Background:         w.Background,
	}
	if w.Stream != nil {
		h.Stream = *w.Stream
	}
	return h, nil
}

package responses

import (
	"encoding/json"
	"fmt"
)

// Parse decodes a POST /v1/responses body into a *Request.
//
// Normalization applied:
//   - string input → []Item{&Message{Role:RoleUser, Content:[]Part{&TextPart{...}}}}
//   - string content inside messages → []Part{&TextPart{...}}
//   - unsupported item types return an explicit error (caller maps to 400)
//   - unsupported tool types return an explicit error (caller maps to 400)
//   - metadata exceeding caps returns an explicit error (caller maps to 400)
func Parse(body []byte) (*Request, error) {
	// wireRequest mirrors the JSON shape before normalization.
	var wire struct {
		Model  string          `json:"model"`
		Input  json.RawMessage `json:"input"`

		Instructions string          `json:"instructions"`
		Tools        json.RawMessage `json:"tools"`
		ToolChoice   json.RawMessage `json:"tool_choice"`

		Temperature     *float64 `json:"temperature"`
		TopP            *float64 `json:"top_p"`
		TopK            *int     `json:"top_k"`
		MaxOutputTokens *int     `json:"max_output_tokens"`

		Text      *TextConfig      `json:"text"`
		Reasoning *ReasoningConfig `json:"reasoning"`

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
	input, err := normalizeInput(wire.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	// Parse tools.
	var tools Tools
	if len(wire.Tools) > 0 && string(wire.Tools) != "null" {
		if err := json.Unmarshal(wire.Tools, &tools); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
	}

	// Parse tool_choice.
	var toolChoice *ToolChoice
	if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
		var tc ToolChoice
		if err := json.Unmarshal(wire.ToolChoice, &tc); err != nil {
			return nil, fmt.Errorf("tool_choice: %w", err)
		}
		toolChoice = &tc
	}

	// Validate metadata.
	if len(wire.Metadata) > 0 {
		if err := validateMetadata(wire.Metadata); err != nil {
			return nil, fmt.Errorf("metadata: %w", err)
		}
	}

	return &Request{
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

// normalizeInput converts the wire input (string or array) to []Item.
// String "hello" → []Item{&Message{Role:RoleUser, Content:[]Part{&TextPart{Text:"hello"}}}}.
func normalizeInput(raw json.RawMessage) ([]Item, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []Item{&Message{
			Role:    RoleUser,
			Content: []Part{&TextPart{Text: s}},
		}}, nil
	}
	return unmarshalItems(raw)
}

// validateMetadata enforces the caps on the metadata map:
// ≤16 entries, key ≤64 chars (DNS-1123 charset), value ≤256 chars (ASCII printable).
func validateMetadata(m map[string]string) error {
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
		if !validMetaKey(k) {
			return fmt.Errorf("key %q contains invalid characters (DNS-1123 charset required)", k)
		}
		if !validMetaValue(v) {
			return fmt.Errorf("value for key %q contains non-printable or disallowed ASCII characters", k)
		}
	}
	return nil
}

func validMetaKey(s string) bool {
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

func validMetaValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// Hints is the minimal set of fields extracted by RoutingHints.
// Fields match those extracted by the Chat Completions parse path, plus
// Responses-specific additions.
type Hints struct {
	Model              string
	Stream             bool
	User               string
	PreviousResponseID string
	Store              *bool
	Background         *bool
}

// RoutingHints extracts the minimum fields needed for route-layer decisions
// without fully parsing the request body. Returns an error only for invalid JSON
// or missing model.
func RoutingHints(body []byte) (*Hints, error) {
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
	h := &Hints{
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

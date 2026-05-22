package v1

import (
	"encoding/json"
	"fmt"
)

// Parse decodes a canonical /v1/generate request body into a *Request.
//
// Normalization applied:
//   - string input → []Item{&Message{Role:RoleUser, Content:[]Part{&TextPart{...}}}}
//   - string content inside messages → []Part{&TextPart{...}}
//   - unsupported item types return an explicit error (caller maps to 400)
//   - unsupported tool types return an explicit error (caller maps to 400)
func Parse(body []byte) (*Request, error) {
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
		User              string            `json:"user"`
		Stream            *bool             `json:"stream"`
		StopSequences     []string          `json:"stop_sequences"`

		Extensions map[string]json.RawMessage `json:"extensions"`
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

	input, err := normalizeInput(wire.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	var tools Tools
	if len(wire.Tools) > 0 && string(wire.Tools) != "null" {
		if err := json.Unmarshal(wire.Tools, &tools); err != nil {
			return nil, fmt.Errorf("tools: %w", err)
		}
	}

	var toolChoice *ToolChoice
	if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
		var tc ToolChoice
		if err := json.Unmarshal(wire.ToolChoice, &tc); err != nil {
			return nil, fmt.Errorf("tool_choice: %w", err)
		}
		toolChoice = &tc
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
		User:              wire.User,
		Stream:            wire.Stream,
		StopSequences:     wire.StopSequences,

		Extensions: wire.Extensions,
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

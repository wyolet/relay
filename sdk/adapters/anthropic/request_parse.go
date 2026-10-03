package anthropic

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseRequest ----

// ParseRequest decodes an Anthropic /v1/messages request body into canonical *v1.Request.
func (AnthropicTranslator) ParseRequest(body []byte) (*v1.Request, error) {
	var wire struct {
		Model         string            `json:"model"`
		System        json.RawMessage   `json:"system"`
		Messages      []json.RawMessage `json:"messages"`
		Tools         []json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage   `json:"tool_choice"`
		MaxTokens     *int              `json:"max_tokens"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		TopK          *int              `json:"top_k"`
		StopSequences []string          `json:"stop_sequences"`
		Stream        bool              `json:"stream"`
		Metadata      json.RawMessage   `json:"metadata"`
		Thinking      json.RawMessage   `json:"thinking"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("anthropic parse_request: %w", err)
	}
	if wire.Model == "" {
		return nil, fmt.Errorf("anthropic parse_request: model is required")
	}

	req := &v1.Request{
		Model: v1.ModelRefs{wire.Model},
	}

	if wire.Stream {
		req.OutputMode = v1.OutputModeStream
	} else {
		req.OutputMode = v1.OutputModeSync
	}

	// system → Instructions
	if len(wire.System) > 0 && string(wire.System) != "null" {
		req.Instructions = anthropicExtractSystemText(wire.System)
	}

	// metadata.user_id → User
	if len(wire.Metadata) > 0 && string(wire.Metadata) != "null" {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(wire.Metadata, &meta); err == nil && meta.UserID != "" {
			req.User = meta.UserID
		}
	}

	// Build model opts.
	opts := &v1.ModelOpts{}
	hasOpts := false

	// Sampling
	sp := &v1.SamplingParams{}
	hasSampling := false
	if wire.Temperature != nil {
		sp.Temperature = wire.Temperature
		hasSampling = true
	}
	if wire.TopP != nil {
		sp.TopP = wire.TopP
		hasSampling = true
	}
	if wire.TopK != nil {
		sp.TopK = wire.TopK
		hasSampling = true
	}
	if wire.MaxTokens != nil {
		sp.MaxTokens = wire.MaxTokens
		hasSampling = true
	}
	if len(wire.StopSequences) > 0 {
		sp.Stop = wire.StopSequences
		hasSampling = true
	}
	if hasSampling {
		opts.Sampling = sp
		hasOpts = true
	}

	// Tools
	if len(wire.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, raw := range wire.Tools {
			tool, err := anthropicParseTool(raw)
			if err != nil {
				return nil, fmt.Errorf("anthropic parse_request: tool: %w", err)
			}
			if tool != nil {
				tc.Definitions = append(tc.Definitions, tool)
			}
		}
		if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
			tc.Choice = anthropicParseToolChoice(wire.ToolChoice)
		}
		req.Tools = tc
	}

	// Thinking → ReasoningConfig
	if len(wire.Thinking) > 0 && string(wire.Thinking) != "null" {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
			Effort       string `json:"effort"`
			Display      string `json:"display"`
		}
		if err := json.Unmarshal(wire.Thinking, &thinking); err == nil {
			switch thinking.Type {
			case "enabled":
				rc := &v1.ReasoningConfig{}
				if thinking.BudgetTokens > 0 {
					rc.BudgetTokens = &thinking.BudgetTokens
				}
				if thinking.Effort != "" {
					rc.Effort = thinking.Effort
				}
				opts.Reasoning = rc
				hasOpts = true
			case "adaptive":
				// Adaptive round-trips as a budget-less ReasoningConfig; display
				// "summarized" surfaces as the canonical Summary request.
				rc := &v1.ReasoningConfig{Effort: thinking.Effort}
				if thinking.Display == "summarized" {
					rc.Summary = "auto"
				}
				opts.Reasoning = rc
				hasOpts = true
			}
		}
	}

	if hasOpts {
		req.ModelConfig = map[string]*v1.ModelOpts{wire.Model: opts}
	}

	// Build Input from messages.
	input, err := anthropicMessagesToCanonical(wire.Messages)
	if err != nil {
		return nil, fmt.Errorf("anthropic parse_request: messages: %w", err)
	}
	req.Input = input

	return req, nil
}

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
//   - model string → []string{model}; array accepted but multiplex rejected at runtime
func Parse(body []byte) (*Request, error) {
	var wire struct {
		Model       ModelRefs                  `json:"model"`
		Input       json.RawMessage            `json:"input"`
		Instructions string                    `json:"instructions"`
		ModelConfig map[string]*ModelOpts      `json:"model_config"`
		CacheConfig *CacheConfig               `json:"cache_config"`
		OutputMode  string                     `json:"output_mode"`
		User        string                     `json:"user"`
		Metadata    map[string]string          `json:"metadata"`
		Extensions  map[string]json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// Validate model.
	if len(wire.Model) == 0 {
		return nil, fmt.Errorf("model required")
	}
	for _, m := range wire.Model {
		if m == "" {
			return nil, fmt.Errorf("model: empty model name")
		}
	}
	if len(wire.Model) > 1 {
		return nil, ErrMultiplexNotImplemented
	}

	// model_config keys must be a subset of model list.
	for k := range wire.ModelConfig {
		found := false
		for _, m := range wire.Model {
			if m == k {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("model_config has key %q not in model list", k)
		}
	}

	if len(wire.Input) == 0 {
		return nil, fmt.Errorf("input is required")
	}

	input, err := normalizeInput(wire.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	return &Request{
		Input:        input,
		Instructions: wire.Instructions,
		Model:        wire.Model,
		ModelConfig:  wire.ModelConfig,
		CacheConfig:  wire.CacheConfig,
		OutputMode:   wire.OutputMode,
		User:         wire.User,
		Metadata:     wire.Metadata,
		Extensions:   wire.Extensions,
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

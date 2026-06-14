package v1

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMultiplexNotImplemented is returned when the request specifies more than
// one model. The wire shape accepts []string; v1 runtime rejects multiplex so
// future support is additive.
var ErrMultiplexNotImplemented = errors.New("multiplex_not_implemented")

const (
	OutputModeSync   = "sync"
	OutputModeStream = "stream"
)

// Request is the canonical inbound request body for /v1/generate.
// Input is always normalized to []Item internally; callers never see the
// string form.
type Request struct {
	Input        []Item `json:"-"` // normalized from string-or-array at parse
	Instructions string `json:"instructions,omitempty"`

	Model       ModelRefs             `json:"model"`
	ModelConfig map[string]*ModelOpts `json:"model_config,omitempty"`

	// Tools is the task-level tool spec (definitions + choice + parallel),
	// shared across every model in a multiplex request. It is NOT per-model:
	// the spec is a property of the task, and the upstream adapter decides how
	// to render it. Per-model knobs (sampling/reasoning/output) live in
	// ModelConfig.
	Tools *ToolsConfig `json:"tools,omitempty"`

	// CacheConfig is the vendor-neutral prompt-cache configuration. Supporting
	// adapters emit breakpoints; others ignore it. See CacheConfig type.
	CacheConfig *CacheConfig `json:"cache_config,omitempty"`

	OutputMode string `json:"output_mode,omitempty"` // "sync" | "stream"

	User       string                     `json:"user,omitempty"`
	Metadata   map[string]string          `json:"metadata,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// MarshalJSON emits a single model as a bare JSON string and multiple as an
// array — symmetric with UnmarshalJSON. The single-string form keeps the wire
// compatible with minimal `{"model": string}` probes on the receiving side.
func (r *Request) MarshalJSON() ([]byte, error) {
	type wire struct {
		Model        ModelRefs                  `json:"model"`
		Instructions string                     `json:"instructions,omitempty"`
		Input        []Item                     `json:"input"`
		ModelConfig  map[string]*ModelOpts      `json:"model_config,omitempty"`
		Tools        *ToolsConfig               `json:"tools,omitempty"`
		CacheConfig  *CacheConfig               `json:"cache_config,omitempty"`
		OutputMode   string                     `json:"output_mode,omitempty"`
		User         string                     `json:"user,omitempty"`
		Metadata     map[string]string          `json:"metadata,omitempty"`
		Extensions   map[string]json.RawMessage `json:"extensions,omitempty"`
	}
	return json.Marshal(wire{
		Model:        r.Model,
		Instructions: r.Instructions,
		Input:        r.Input,
		ModelConfig:  r.ModelConfig,
		Tools:        r.Tools,
		CacheConfig:  r.CacheConfig,
		OutputMode:   r.OutputMode,
		User:         r.User,
		Metadata:     r.Metadata,
		Extensions:   r.Extensions,
	})
}

// ModelRefs is the string-or-array union for the model field.
// Unmarshal accepts a JSON string OR a JSON array of strings.
// Always normalized to []string internally.
type ModelRefs []string

// MarshalJSON emits a single ref as a bare string, multiple as an array.
func (m ModelRefs) MarshalJSON() ([]byte, error) {
	if len(m) == 1 {
		return json.Marshal(m[0])
	}
	return json.Marshal([]string(m))
}

func (m *ModelRefs) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*m = nil
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("model: %w", err)
		}
		*m = ModelRefs{s}
		return nil
	}
	if data[0] == '[' {
		var ss []string
		if err := json.Unmarshal(data, &ss); err != nil {
			return fmt.Errorf("model: %w", err)
		}
		*m = ModelRefs(ss)
		return nil
	}
	return fmt.Errorf("model: expected string or array, got %q", string(data[:min(len(data), 32)]))
}

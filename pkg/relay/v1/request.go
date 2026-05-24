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

	// CacheConfig is the vendor-neutral prompt-cache configuration. Supporting
	// adapters emit breakpoints; others ignore it. See CacheConfig type.
	CacheConfig *CacheConfig `json:"cache_config,omitempty"`

	OutputMode string `json:"output_mode,omitempty"` // "sync" | "stream"

	User       string                     `json:"user,omitempty"`
	Metadata   map[string]string          `json:"metadata,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// ModelRefs is the string-or-array union for the model field.
// Unmarshal accepts a JSON string OR a JSON array of strings.
// Always normalized to []string internally.
type ModelRefs []string

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

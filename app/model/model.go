// Package model is the domain layer for the Model entity — a published LLM
// behind a Provider. The wire shape sent upstream is Spec.UpstreamName; the
// catalog handle is Meta.Name (slug) with optional Spec.Aliases.
//
// Pricing and RateLimit attachments are deferred until those packages land.
package model

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Model is the published model definition.
type Model struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec is the body. Cross-refs are stored as ids.
type Spec struct {
	// ProviderID identifies the owning Provider. Validated against the
	// snapshot at composition time; only structural (uuid) here.
	ProviderID string `json:"providerId" yaml:"providerId" validate:"required,uuid"`

	// UpstreamName is the model identifier sent to the provider's API
	// (e.g. "gpt-4o-2024-08-06"). Required.
	UpstreamName string `json:"upstreamName" yaml:"upstreamName" validate:"required"`

	Family  string `json:"family,omitempty"  yaml:"family,omitempty"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	Capabilities Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Modalities   Modalities   `json:"modalities,omitempty"   yaml:"modalities,omitempty"`

	// Context window split. ContextWindowTotal is canonical; the Input/Output
	// pair is informational for models that publish a soft cap.
	ContextWindowInput  int `json:"contextWindowInput,omitempty"  yaml:"contextWindowInput,omitempty"  validate:"omitempty,gte=0"`
	ContextWindowOutput int `json:"contextWindowOutput,omitempty" yaml:"contextWindowOutput,omitempty" validate:"omitempty,gte=0"`
	ContextWindowTotal  int `json:"contextWindowTotal,omitempty"  yaml:"contextWindowTotal,omitempty"  validate:"omitempty,gte=0"`
	MaxOutputTokens     int `json:"maxOutputTokens,omitempty"     yaml:"maxOutputTokens,omitempty"     validate:"omitempty,gte=0"`

	KnowledgeCutoff string       `json:"knowledgeCutoff,omitempty" yaml:"knowledgeCutoff,omitempty"`
	ReleaseDate     string       `json:"releaseDate,omitempty"     yaml:"releaseDate,omitempty"`
	DeprecationDate string       `json:"deprecationDate,omitempty" yaml:"deprecationDate,omitempty"`
	Deprecation     *Deprecation `json:"deprecation,omitempty"     yaml:"deprecation,omitempty"`

	Aliases              []string `json:"aliases,omitempty"              yaml:"aliases,omitempty"`
	Tags                 []string `json:"tags,omitempty"                 yaml:"tags,omitempty"`
	Documentation        string   `json:"documentation,omitempty"        yaml:"documentation,omitempty"`
	License              string   `json:"license,omitempty"              yaml:"license,omitempty"`
	ProviderModelPageURL string   `json:"providerModelPageURL,omitempty" yaml:"providerModelPageURL,omitempty" validate:"omitempty,http_url"`

	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"` // nil = true
}

// Capabilities is the bag-of-bools feature flags every model declares.
// Open-ended over time; new flags can be added without a migration since
// the whole struct lives in JSONB.
type Capabilities struct {
	Chat              bool `json:"chat,omitempty"              yaml:"chat,omitempty"`
	Embeddings        bool `json:"embeddings,omitempty"        yaml:"embeddings,omitempty"`
	Streaming         bool `json:"streaming,omitempty"         yaml:"streaming,omitempty"`
	Tools             bool `json:"tools,omitempty"             yaml:"tools,omitempty"`
	ParallelTools     bool `json:"parallelTools,omitempty"     yaml:"parallelTools,omitempty"`
	Vision            bool `json:"vision,omitempty"            yaml:"vision,omitempty"`
	Audio             bool `json:"audio,omitempty"             yaml:"audio,omitempty"`
	PromptCache       bool `json:"promptCache,omitempty"       yaml:"promptCache,omitempty"`
	Reasoning         bool `json:"reasoning,omitempty"         yaml:"reasoning,omitempty"`
	JSONMode          bool `json:"jsonMode,omitempty"          yaml:"jsonMode,omitempty"`
	StructuredOutputs bool `json:"structuredOutputs,omitempty" yaml:"structuredOutputs,omitempty"`
	Batch             bool `json:"batch,omitempty"             yaml:"batch,omitempty"`
	ComputerUse       bool `json:"computerUse,omitempty"       yaml:"computerUse,omitempty"`
	WebSearch         bool `json:"webSearch,omitempty"         yaml:"webSearch,omitempty"`
	FileInput         bool `json:"fileInput,omitempty"         yaml:"fileInput,omitempty"`
	AudioInput        bool `json:"audioInput,omitempty"        yaml:"audioInput,omitempty"`
	AudioOutput       bool `json:"audioOutput,omitempty"       yaml:"audioOutput,omitempty"`
	SystemMessages    bool `json:"systemMessages,omitempty"    yaml:"systemMessages,omitempty"`
	AssistantPrefill  bool `json:"assistantPrefill,omitempty"  yaml:"assistantPrefill,omitempty"`
}

// Modalities lists input/output media types ("text", "image", "audio", ...).
type Modalities struct {
	Input  []string `json:"input,omitempty"  yaml:"input,omitempty"`
	Output []string `json:"output,omitempty" yaml:"output,omitempty"`
}

// Deprecation describes the lifecycle state of a Model.
type Deprecation struct {
	Status      DeprecationStatus `json:"status,omitempty"      yaml:"status,omitempty"      validate:"omitempty,oneof=active deprecated sunset"`
	SunsetDate  string            `json:"sunsetDate,omitempty"  yaml:"sunsetDate,omitempty"`
	Replacement string            `json:"replacement,omitempty" yaml:"replacement,omitempty" validate:"omitempty,uuid"`
}

// DeprecationStatus enumerates the lifecycle states.
type DeprecationStatus string

const (
	DeprecationActive     DeprecationStatus = "active"
	DeprecationDeprecated DeprecationStatus = "deprecated"
	DeprecationSunset     DeprecationStatus = "sunset"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (m *Model) IsEnabled() bool { return m.Spec.Enabled == nil || *m.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Model-specific invariants:
//   - Owner is required and must be provider-kind with a non-empty ID.
//     Models always belong to a Provider; "system" or user-created models
//     are exposed by attaching them to a relay-owned Provider (e.g. "wr"),
//     not by changing the owner kind.
//   - Aliases are non-empty, case-insensitively unique, and must not
//     collide with the Model's own Meta.Name.
//
// Cross-entity checks (ProviderID resolves to a real Provider; Owner.ID
// matches Spec.ProviderID; Deprecation.Replacement resolves to a real Model;
// alias uniqueness across Models) live in the composition layer.
func (m *Model) Validate() error {
	if err := meta.Validator.Struct(m); err != nil {
		return err
	}
	if m.Meta.Owner.Kind != meta.OwnerProvider {
		return fmt.Errorf("model %q: owner.kind must be provider, got %q", m.Meta.Name, m.Meta.Owner.Kind)
	}
	if m.Meta.Owner.ID == "" {
		return fmt.Errorf("model %q: owner.id is required (provider id)", m.Meta.Name)
	}
	ownName := lower(m.Meta.Name)
	seen := make(map[string]struct{}, len(m.Spec.Aliases))
	for _, a := range m.Spec.Aliases {
		if a == "" {
			return fmt.Errorf("model %q: alias is empty", m.Meta.Name)
		}
		key := lower(a)
		if key == ownName {
			return fmt.Errorf("model %q: alias %q collides with the model's own name", m.Meta.Name, a)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("model %q: duplicate alias %q", m.Meta.Name, a)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// lower is a tiny ascii-lowercase to avoid importing strings just for one use.
func lower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

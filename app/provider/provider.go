// Package provider is the domain layer for the Provider entity — a definition
// of an upstream LLM API endpoint.
//
// types.go holds the domain struct and validation. The wire-shape family
// (OpenAI / Anthropic / Ollama) is selected in code from the model name
// prefix, not stored on the row.
package provider

import "github.com/wyolet/relay/app/meta"

// Provider is the configured upstream endpoint.
type Provider struct {
	Meta meta.Metadata
	Spec Spec
}

// Spec is the body. Optional fields are pointers so absence is distinguishable
// from zero.
type Spec struct {
	BaseURL         string `validate:"required,http_url"`
	Default         bool
	DefaultPolicyID string `validate:"omitempty,uuid"`
	DefaultTier     string `validate:"omitempty,slug"`
	Enabled         *bool  // nil = true

	// Display metadata — operator-set, optional. Free-text.
	HomepageURL   string `validate:"omitempty,http_url"`
	DocsURL       string `validate:"omitempty,http_url"`
	ConsoleURL    string `validate:"omitempty,http_url"`
	StatusPageURL string `validate:"omitempty,http_url"`
	LogoURL       string `validate:"omitempty,http_url"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Provider) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator. Cross-entity
// checks (DefaultPolicyID resolves to a Policy that targets this Provider,
// at-most-one-default-Provider) live in the composition layer.
func (p *Provider) Validate() error { return meta.Validator.Struct(p) }

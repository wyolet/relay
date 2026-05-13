// Package provider is the domain layer for the Provider entity — a definition
// of an upstream LLM API endpoint.
//
// types.go holds the domain struct and validation. The wire-shape family
// (OpenAI / Anthropic / Ollama) is selected in code from the model name
// prefix, not stored on the row.
package provider

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Provider is the configured upstream endpoint.
type Provider struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec is the body. Optional fields are pointers so absence is distinguishable
// from zero.
type Spec struct {
	BaseURL         string `json:"baseURL"                   yaml:"baseURL"                   validate:"required,http_url"`
	Default         bool   `json:"default,omitempty"         yaml:"default,omitempty"`
	DefaultPolicyID string `json:"defaultPolicyId,omitempty" yaml:"defaultPolicyId,omitempty" validate:"omitempty,uuid"`
	DefaultTier     string `json:"defaultTier,omitempty"     yaml:"defaultTier,omitempty"     validate:"omitempty,slug"`
	Enabled         *bool  `json:"enabled,omitempty"         yaml:"enabled,omitempty"` // nil = true

	// Display metadata — operator-set, optional. Free-text.
	HomepageURL   string `json:"homepageURL,omitempty"   yaml:"homepageURL,omitempty"   validate:"omitempty,http_url"`
	DocsURL       string `json:"docsURL,omitempty"       yaml:"docsURL,omitempty"       validate:"omitempty,http_url"`
	ConsoleURL    string `json:"consoleURL,omitempty"    yaml:"consoleURL,omitempty"    validate:"omitempty,http_url"`
	StatusPageURL string `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty" validate:"omitempty,http_url"`
	LogoURL       string `json:"logoURL,omitempty"       yaml:"logoURL,omitempty"       validate:"omitempty,http_url"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Provider) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Provider-specific invariant that Owner.Kind is system (Providers are
// not user- or provider-owned). Cross-entity checks (DefaultPolicyID resolves
// to a Policy that targets this Provider, at-most-one-default-Provider) live
// in the composition layer.
func (p *Provider) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	if p.Meta.Owner.Kind != "" && p.Meta.Owner.Kind != meta.OwnerSystem {
		return fmt.Errorf("provider %q: owner.kind must be system, got %q", p.Meta.Name, p.Meta.Owner.Kind)
	}
	return nil
}

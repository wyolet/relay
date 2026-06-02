// Package provider is the domain layer for the Provider entity — the
// vendor / author of a Model. Anthropic, OpenAI, Meta-Llama, Mistral,
// Google. Operational concerns (BaseURL, auth) live on Host, not Provider.
// Provider is essentially display metadata + the identity Model rows hang
// their Owner.ID off.
package provider

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Provider is the vendor that authors Models.
type Provider struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec is display-only. There is no BaseURL on Provider — that lives on
// Host, since the same Provider (e.g. Anthropic) is served by multiple
// Hosts (Anthropic direct, Bedrock, Vertex). Per-Host serving info lives
// on standalone HostBinding entities (app/binding).
type Spec struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"` // nil = true

	// Display metadata — operator-set, optional.
	HomepageURL   string     `json:"homepageURL,omitempty"   yaml:"homepageURL,omitempty"   validate:"omitempty,http_url"`
	DocsURL       string     `json:"docsURL,omitempty"       yaml:"docsURL,omitempty"       validate:"omitempty,http_url"`
	StatusPageURL string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty" validate:"omitempty,http_url"`
	Icon          *meta.Icon `json:"icon,omitempty"          yaml:"icon,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Provider) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Provider-specific invariant that Owner.Kind, when set, is system.
// Empty is accepted — Providers are inherently system-defined, so YAML
// authors are not required to spell that out.
func (p *Provider) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	if p.Meta.Owner.Kind != "" && p.Meta.Owner.Kind != meta.OwnerSystem {
		return fmt.Errorf("provider %q: owner.kind must be system or empty, got %q", p.Meta.Name, p.Meta.Owner.Kind)
	}
	return nil
}

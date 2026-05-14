// Package host is the domain layer for the Host entity — a serving endpoint
// the relay forwards requests to. Examples: openai-direct (api.openai.com),
// bedrock-us-east-1 (AWS Bedrock in that region), azure-prod-eastus (one
// Azure OpenAI deployment), together, groq, fireworks. Each represents a
// distinct URL + auth combination.
//
// Hosts are operator-defined infrastructure (Owner.Kind=system). HostKeys
// belong to Hosts. Models declare which Hosts can serve them via
// Spec.Hosts []HostBinding.
//
// Storage: not yet wired to PG (needs a new hosts table + sqlc queries).
// The catalog composition layer consumes a HostLister interface; tests
// supply in-memory fakes. The real Store will land alongside the schema
// migration.
package host

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Host is a single upstream serving endpoint.
type Host struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the routing + display fields.
type Spec struct {
	// BaseURL is the upstream root. Required.
	BaseURL string `json:"baseURL" yaml:"baseURL" validate:"required,http_url"`

	// Backend is the free-form bag of backend-specific config (Bedrock region,
	// Azure deployment, Vertex project/location, etc.). Each provider client
	// reads the keys it needs and ignores the rest. Optional.
	Backend map[string]string `json:"backend,omitempty" yaml:"backend,omitempty"`

	// Policies is the host's menu of upstream tier Policy ids a HostKey can
	// mirror. Every entry must be a Policy with Meta.Owner = {kind:host, id:
	// <this host's id>}. The composition layer enforces the menu invariant.
	Policies []string `json:"policies,omitempty" yaml:"policies,omitempty"`

	// DefaultPolicy is the Policy id from Policies a HostKey inherits when
	// its Spec.PolicyID is empty. Must be one of Policies. Optional — if
	// empty, HostKey.Spec.PolicyID becomes required.
	DefaultPolicy string `json:"defaultPolicy,omitempty" yaml:"defaultPolicy,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Display metadata — operator-set, optional.
	HomepageURL   string `json:"homepageURL,omitempty"   yaml:"homepageURL,omitempty"   validate:"omitempty,http_url"`
	DocsURL       string `json:"docsURL,omitempty"       yaml:"docsURL,omitempty"       validate:"omitempty,http_url"`
	ConsoleURL    string `json:"consoleURL,omitempty"    yaml:"consoleURL,omitempty"    validate:"omitempty,http_url"`
	StatusPageURL string `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty" validate:"omitempty,http_url"`
	Icon          *meta.Icon `json:"icon,omitempty"          yaml:"icon,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (h *Host) IsEnabled() bool { return h.Spec.Enabled == nil || *h.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Host-specific invariant that Owner.Kind is system.
func (h *Host) Validate() error {
	if err := meta.Validator.Struct(h); err != nil {
		return err
	}
	if h.Meta.Owner.Kind != "" && h.Meta.Owner.Kind != meta.OwnerSystem {
		return fmt.Errorf("host %q: owner.kind must be system, got %q", h.Meta.Name, h.Meta.Owner.Kind)
	}
	return nil
}

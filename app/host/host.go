// Package host is the domain layer for the Host entity — a serving endpoint
// the relay forwards requests to. Examples: openai-direct (api.openai.com),
// bedrock-us-east-1 (AWS Bedrock in that region), azure-prod-eastus (one
// Azure OpenAI deployment), together, groq, fireworks. Each represents a
// distinct URL + auth combination.
//
// Hosts are operator-defined infrastructure (Owner.Kind=system). HostKeys
// belong to Hosts. Which Hosts can serve a Model is declared by standalone
// HostBinding entities (app/binding), not on the Model.
//
// Storage: not yet wired to PG (needs a new hosts table + sqlc queries).
// The catalog composition layer consumes a HostLister interface; tests
// supply in-memory fakes. The real Store will land alongside the schema
// migration.
package host

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/meta"
)

// Host is a single upstream serving endpoint.
//
// Spec is desired state (operator-managed, persisted in PG). Status is
// observed runtime state (data-plane-managed, NOT persisted — overlaid from
// kv at read time, like HostKey.Policies). The split mirrors the k8s
// spec/status convention.
type Host struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`

	// Status is runtime-observed health, overlaid by the control-API enrich
	// step from the host-health store. nil when no observation exists yet
	// (no traffic since boot, or the record TTL'd out) — the UI shows
	// "unknown". Never persisted and never loaded from YAML.
	Status *Status `json:"status,omitempty" yaml:"-"`
}

// HostHealth is the observed reachability of a host's upstream.
type HostHealth string

const (
	// HealthUnknown — no observation yet (no traffic since boot or the record
	// TTL'd out). The zero value.
	HealthUnknown HostHealth = ""
	// HealthHealthy — the upstream was reached on the most recent request
	// (any HTTP response, even 4xx/5xx — reachability, not success).
	HealthHealthy HostHealth = "healthy"
	// HealthUnreachable — the most recent request failed to establish a
	// connection (dial refused / DNS / TLS) after the pipeline's retries.
	HealthUnreachable HostHealth = "unreachable"
)

// Status is runtime-observed health for a Host. Written by the data plane on
// each request outcome, read by the admin API. Not persisted.
type Status struct {
	Health              HostHealth `json:"health" doc:"unknown | healthy | unreachable."`
	LastError           string     `json:"lastError,omitempty" doc:"Last dial-failure error excerpt; set while unreachable."`
	ConsecutiveFailures int        `json:"consecutiveFailures,omitempty" doc:"Consecutive dial failures; 0 when healthy."`
	LastTransition      time.Time  `json:"lastTransition,omitempty" doc:"When health was last recorded."`
	LastSuccess         time.Time  `json:"lastSuccess,omitempty" doc:"When the host was last reachable."`
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

	// NoAuth marks an upstream that needs no API key (a self-hosted Ollama on
	// the operator's network). When set, routing injects a synthetic anonymous
	// key for this host instead of requiring a real HostKey, and the adapter
	// sends no Authorization header. Operator-managed (per-deployment), not a
	// catalog-fixed property — Ollama Cloud still needs a key.
	NoAuth bool `json:"noAuth,omitempty" yaml:"noAuth,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Display metadata — operator-set, optional.
	HomepageURL   string     `json:"homepageURL,omitempty"   yaml:"homepageURL,omitempty"   validate:"omitempty,http_url"`
	DocsURL       string     `json:"docsURL,omitempty"       yaml:"docsURL,omitempty"       validate:"omitempty,http_url"`
	ConsoleURL    string     `json:"consoleURL,omitempty"    yaml:"consoleURL,omitempty"    validate:"omitempty,http_url"`
	StatusPageURL string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty" validate:"omitempty,http_url"`
	Icon          *meta.Icon `json:"icon,omitempty"          yaml:"icon,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (h *Host) IsEnabled() bool { return h.Spec.Enabled == nil || *h.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Host-specific owner rule. A Host is normally system-defined
// infrastructure (empty owner defaults to system), but it may also be
// user-owned: some hosts are per-deployment (e.g. a self-hosted Ollama whose
// baseURL only the operator knows), and user ownership is what lets the
// operator edit them through the standard CRUD path. Governance still hard-
// blocks edits/deletes on system-owned rows, so relaxing this does not expose
// catalog-fixed hosts. Provider/host owner kinds remain nonsensical for a Host.
func (h *Host) Validate() error {
	if err := meta.Validator.Struct(h); err != nil {
		return err
	}
	switch h.Meta.Owner.Kind {
	case "", meta.OwnerSystem, meta.OwnerUser:
	default:
		return fmt.Errorf("host %q: owner.kind must be system, user, or empty, got %q", h.Meta.Name, h.Meta.Owner.Kind)
	}
	return nil
}

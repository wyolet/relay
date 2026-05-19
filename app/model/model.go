// Package model is the domain layer for the Model entity — a container
// for a family of dated checkpoints (Snapshots) served via one or more
// Hosts. Meta.Name is the admin / catalog slug; customers address the
// model by sending a Snapshot.Name in the request body. Snapshot.Pointer
// names the default snapshot displayed in /v1/models.
//
// Pricing and RateLimit attachments are deferred until those packages land.
package model

import (
	"fmt"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/meta"
)

// Model is the published model definition.
type Model struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec is the body. Cross-refs are stored as ids. The owning Provider
// (vendor) id lives on Meta.Owner.ID (Owner.Kind is always "provider");
// there is no separate spec.providerId field.
//
// Per-Host serving info (which Hosts can serve this Model and what name
// each one calls the model upstream) lives in Spec.Hosts.
type Spec struct {
	// Hosts is the list of HostBindings — one per Host that serves this
	// Model. At least one is required for the Model to be callable.
	Hosts []HostBinding `json:"hosts" yaml:"hosts" validate:"required,min=1,dive"`

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

	Tags                 []string `json:"tags,omitempty"                 yaml:"tags,omitempty"`
	Documentation        string   `json:"documentation,omitempty"        yaml:"documentation,omitempty"`
	License              string   `json:"license,omitempty"              yaml:"license,omitempty"`
	ProviderModelPageURL string   `json:"providerModelPageURL,omitempty" yaml:"providerModelPageURL,omitempty" validate:"omitempty,http_url"`

	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"` // nil = true

	// Snapshots are the dated checkpoints this Model exposes. Every Model has
	// at least one. Pointer names the snapshot the bare Model name resolves
	// to at request time.
	Snapshots []Snapshot `json:"snapshots" yaml:"snapshots" validate:"required,min=1,dive"`
	Pointer   string     `json:"pointer"   yaml:"pointer"   validate:"required"`
}

// Snapshot is a dated checkpoint of a Model. Name is the customer-facing
// identifier (DNS-1123 slug; matches what the SDK sends in the `model`
// field). OriginalName is the upstream wire name; when omitted it
// defaults to Name — the common case where the slug-safe name already
// matches what the provider expects.
type Snapshot struct {
	Name         string `json:"name"                   yaml:"name"                   validate:"required,hostname_rfc1123"`
	ReleasedAt   string `json:"releasedAt,omitempty"   yaml:"releasedAt,omitempty"`
	OriginalName string `json:"originalName,omitempty" yaml:"originalName,omitempty"`
}

// Upstream returns the wire name to send to the provider — OriginalName
// when set, otherwise Name.
func (s Snapshot) Upstream() string {
	if s.OriginalName != "" {
		return s.OriginalName
	}
	return s.Name
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

// HostBinding declares that a Model is callable via a particular Host,
// what name the Host's upstream API expects, and which wire-protocol
// adapter the relay must use to talk to it. One Model lists one binding
// per Host that serves it.
type HostBinding struct {
	HostID       string        `json:"hostId"             yaml:"hostId"             validate:"required,uuid"`
	UpstreamName string        `json:"upstreamName"       yaml:"upstreamName"       validate:"required"`
	Adapter      adapters.Kind  `json:"adapter"            yaml:"adapter"            validate:"required,oneof=openai anthropic"`
	Enabled      *bool         `json:"enabled,omitempty"  yaml:"enabled,omitempty"` // nil = true
}

// IsEnabled returns true when the binding's Enabled is unset or explicitly true.
func (b HostBinding) IsEnabled() bool { return b.Enabled == nil || *b.Enabled }

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
//     Models always belong to a Provider.
//   - Host bindings are unique by HostID.
//   - Snapshot names are unique within the model and Pointer must name
//     one of them.
//
// Cross-entity checks (Owner.ID resolves to a real Provider; Deprecation.
// Replacement resolves to a real Model; snapshot-name uniqueness across
// the catalog) live in the composition layer.
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
	hosts := make(map[string]struct{}, len(m.Spec.Hosts))
	for _, b := range m.Spec.Hosts {
		if _, dup := hosts[b.HostID]; dup {
			return fmt.Errorf("model %q: duplicate host binding %q", m.Meta.Name, b.HostID)
		}
		hosts[b.HostID] = struct{}{}
	}
	snaps := make(map[string]struct{}, len(m.Spec.Snapshots))
	for _, s := range m.Spec.Snapshots {
		key := lower(s.Name)
		if _, dup := snaps[key]; dup {
			return fmt.Errorf("model %q: duplicate snapshot %q", m.Meta.Name, s.Name)
		}
		snaps[key] = struct{}{}
	}
	if _, ok := snaps[lower(m.Spec.Pointer)]; !ok {
		return fmt.Errorf("model %q: pointer %q does not match any snapshot", m.Meta.Name, m.Spec.Pointer)
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

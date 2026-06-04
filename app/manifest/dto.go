package manifest

import (
	"encoding/json"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

// WireMeta is the metadata block shared by all wire DTOs. ID is optional on
// create (server stamps a UUIDv7); required on update.
type WireMeta struct {
	ID          string            `json:"id,omitempty"          yaml:"id,omitempty"`
	Name        string            `json:"name"                  yaml:"name"`
	DisplayName string            `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Owner       WireOwner         `json:"owner,omitempty"       yaml:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"      yaml:"labels,omitempty"`
}

// WireOwner is the wire form of meta.Owner. The referenced row is named —
// translate functions resolve Name → id when producing the domain shape,
// and reverse-resolve id → Name when emitting the wire shape. ID is the
// id-form for API clients that already hold a UUID; either field is
// accepted on read, with ID taking precedence.
type WireOwner struct {
	Kind meta.OwnerKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Name string         `json:"name,omitempty" yaml:"name,omitempty"`
	ID   string         `json:"id,omitempty"   yaml:"id,omitempty"`
}

// ref returns whichever identifier the caller supplied — id takes
// precedence so API roundtrips that emit id keep working without a name
// resolver. Translate code treats the result as a name-or-id and runs
// the resolver against it.
func (o WireOwner) ref() string {
	if o.ID != "" {
		return o.ID
	}
	return o.Name
}

func (w WireMeta) toMeta() meta.Metadata {
	return meta.Metadata{
		ID:          w.ID,
		Name:        w.Name,
		DisplayName: w.DisplayName,
		Description: w.Description,
		Owner:       meta.Owner{Kind: w.Owner.Kind, ID: w.Owner.ref()},
		Labels:      w.Labels,
	}
}

func metaToWire(m meta.Metadata) WireMeta {
	return WireMeta{
		ID:          m.ID,
		Name:        m.Name,
		DisplayName: m.DisplayName,
		Description: m.Description,
		Owner:       WireOwner{Kind: m.Owner.Kind, Name: m.Owner.ID},
		Labels:      m.Labels,
	}
}

// ProviderDTO is the wire form of a Provider. No cross-refs — Provider has
// only display fields in its spec.
type ProviderDTO struct {
	APIVersion string       `json:"apiVersion" yaml:"apiVersion"`
	Kind       string       `json:"kind"       yaml:"kind"`
	Metadata   WireMeta     `json:"metadata"   yaml:"metadata"`
	Spec       ProviderSpec `json:"spec"      yaml:"spec"`
}

type ProviderSpec struct {
	Enabled       *bool      `json:"enabled,omitempty"       yaml:"enabled,omitempty"`
	HomepageURL   string     `json:"homepageURL,omitempty"   yaml:"homepageURL,omitempty"`
	DocsURL       string     `json:"docsURL,omitempty"       yaml:"docsURL,omitempty"`
	StatusPageURL string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty"`
	Icon          *meta.Icon `json:"icon,omitempty"          yaml:"icon,omitempty"`
}

// HostDTO is the wire form of a Host.
type HostDTO struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind"       yaml:"kind"`
	Metadata   WireMeta `json:"metadata"   yaml:"metadata"`
	Spec       HostSpec `json:"spec"       yaml:"spec"`
}

type HostSpec struct {
	BaseURL string            `json:"baseURL"               yaml:"baseURL"`
	Backend map[string]string `json:"backend,omitempty"     yaml:"backend,omitempty"`
	// Policies holds policy *names* (wire form), resolved to ids on parse.
	Policies []string `json:"policies,omitempty"    yaml:"policies,omitempty"`
	// DefaultPolicy is a policy *name* (wire form) referencing one of Policies.
	DefaultPolicy string     `json:"defaultPolicy,omitempty" yaml:"defaultPolicy,omitempty"`
	NoAuth        bool       `json:"noAuth,omitempty"      yaml:"noAuth,omitempty"`
	Enabled       *bool      `json:"enabled,omitempty"     yaml:"enabled,omitempty"`
	HomepageURL   string     `json:"homepageURL,omitempty" yaml:"homepageURL,omitempty"`
	DocsURL       string     `json:"docsURL,omitempty"     yaml:"docsURL,omitempty"`
	ConsoleURL    string     `json:"consoleURL,omitempty"  yaml:"consoleURL,omitempty"`
	StatusPageURL string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty"`
	Icon          *meta.Icon `json:"icon,omitempty"        yaml:"icon,omitempty"`
}

// ModelDTO is the wire form of a Model.
// Owner.ID in the wire form should be the provider *name*; translate resolves it.
type ModelDTO struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind"       yaml:"kind"`
	Metadata   WireMeta  `json:"metadata"   yaml:"metadata"`
	Spec       ModelSpec `json:"spec"       yaml:"spec"`
}

type ModelSpec struct {
	Family  string `json:"family,omitempty"  yaml:"family,omitempty"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	Capabilities model.Capabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Modalities   model.Modalities   `json:"modalities,omitempty"   yaml:"modalities,omitempty"`

	ContextWindowInput  int `json:"contextWindowInput,omitempty"  yaml:"contextWindowInput,omitempty"`
	ContextWindowOutput int `json:"contextWindowOutput,omitempty" yaml:"contextWindowOutput,omitempty"`
	ContextWindowTotal  int `json:"contextWindowTotal,omitempty"  yaml:"contextWindowTotal,omitempty"`
	MaxOutputTokens     int `json:"maxOutputTokens,omitempty"     yaml:"maxOutputTokens,omitempty"`

	KnowledgeCutoff string             `json:"knowledgeCutoff,omitempty" yaml:"knowledgeCutoff,omitempty"`
	ReleaseDate     string             `json:"releaseDate,omitempty"     yaml:"releaseDate,omitempty"`
	DeprecationDate string             `json:"deprecationDate,omitempty" yaml:"deprecationDate,omitempty"`
	Deprecation     *model.Deprecation `json:"deprecation,omitempty"     yaml:"deprecation,omitempty"`

	Tags                 []string `json:"tags,omitempty"                 yaml:"tags,omitempty"`
	Documentation        string   `json:"documentation,omitempty"        yaml:"documentation,omitempty"`
	License              string   `json:"license,omitempty"              yaml:"license,omitempty"`
	ProviderModelPageURL string   `json:"providerModelPageURL,omitempty" yaml:"providerModelPageURL,omitempty"`

	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	Snapshots []model.Snapshot `json:"snapshots" yaml:"snapshots"`
	Pointer   string           `json:"pointer"   yaml:"pointer"`
}

// HostBindingDTO is the top-level wire form of a standalone HostBinding entity.
// Model and Host carry *names* (wire form); translate resolves them to ids.
type HostBindingDTO struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind"       yaml:"kind"`
	Metadata   WireMeta        `json:"metadata"   yaml:"metadata"`
	Spec       HostBindingSpec `json:"spec"       yaml:"spec"`
}

// HostBindingSpec is the spec block of a standalone HostBinding.
type HostBindingSpec struct {
	// Model is the model *name* (wire form).
	Model string `json:"model" yaml:"model"`
	// Host is the host *name* (wire form).
	Host         string `json:"host"                   yaml:"host"`
	Adapter      string `json:"adapter"                yaml:"adapter"`
	UpstreamName string `json:"upstreamName,omitempty" yaml:"upstreamName,omitempty"`
	// Pricing is an optional pricing *name* (wire form).
	Pricing   string   `json:"pricing,omitempty"  yaml:"pricing,omitempty"`
	Enabled   *bool    `json:"enabled,omitempty"  yaml:"enabled,omitempty"`
	Snapshots []string `json:"snapshots,omitempty" yaml:"snapshots,omitempty"`
}

// HostKeyDTO is the wire form of a HostKey. Spec.HostID is a host *name* here.
type HostKeyDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       HostKeySpec `json:"spec"       yaml:"spec"`
}

type HostKeySpec struct {
	// HostID and PolicyID carry *names* on the wire; translate resolves
	// to ids when producing the domain shape.
	HostID      string           `json:"hostId"                yaml:"hostId"`
	PolicyID    string           `json:"policyId"              yaml:"policyId"`
	ValueFrom   HostKeyValueFrom `json:"valueFrom"             yaml:"valueFrom"`
	DefaultTier string           `json:"defaultTier,omitempty" yaml:"defaultTier,omitempty"`
	Enabled     *bool            `json:"enabled,omitempty"     yaml:"enabled,omitempty"`
	Value       string           `json:"-"                     yaml:"value,omitempty"`
}

type HostKeyValueFrom struct {
	Kind string `json:"kind"          yaml:"kind"`
	Env  string `json:"env,omitempty" yaml:"env,omitempty"`
}

// PolicyDTO carries policy-level model-handling flags + the grant list.
type PolicyDTO struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind"       yaml:"kind"`
	Metadata   WireMeta   `json:"metadata"   yaml:"metadata"`
	Spec       PolicySpec `json:"spec"       yaml:"spec"`
}

type PolicySpec struct {
	// Models holds model *names* (wire form).
	Models []string `json:"models,omitempty"    yaml:"models,omitempty"`
	// HostKeys holds host-key *names* (wire form).
	HostKeys []string `json:"hostKeys,omitempty"  yaml:"hostKeys,omitempty"`

	// RateLimit holds a rate-limit *name* (wire form). Mutually exclusive
	// with RLBindings.
	RateLimit string `json:"rateLimit,omitempty" yaml:"rateLimit,omitempty"`

	// RLBindings is the per-model rate-limit map (wire form). Each entry's
	// RateLimit field carries a *name* that translate resolves to an id.
	RLBindings []RLBindingDTO `json:"rlBindings,omitempty" yaml:"rlBindings,omitempty"`

	KeySelection          string `json:"keySelection,omitempty"          yaml:"keySelection,omitempty"`
	SkipDefaultLimits     bool   `json:"skipDefaultLimits,omitempty"     yaml:"skipDefaultLimits,omitempty"`
	IncludeDeprecated     bool   `json:"includeDeprecated,omitempty"     yaml:"includeDeprecated,omitempty"`
	Enabled               *bool  `json:"enabled,omitempty"               yaml:"enabled,omitempty"`
	PayloadLoggingEnabled bool   `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty"`
}

// RLBindingDTO is the wire form of a policy.RLBinding. Models are modelref
// DSL strings carried verbatim; RateLimit is a name resolved to an id.
type RLBindingDTO struct {
	Models    []string `json:"models"    yaml:"models"`
	RateLimit string   `json:"rateLimit" yaml:"rateLimit"`
}

// RateLimitDTO is the wire form of a RateLimit. No cross-refs.
type RateLimitDTO struct {
	APIVersion string        `json:"apiVersion" yaml:"apiVersion"`
	Kind       string        `json:"kind"       yaml:"kind"`
	Metadata   WireMeta      `json:"metadata"   yaml:"metadata"`
	Spec       RateLimitSpec `json:"spec"      yaml:"spec"`
}

type RateLimitSpec struct {
	Rules   []RateLimitRule `json:"rules"             yaml:"rules"`
	Enabled *bool           `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

type RateLimitRule struct {
	Meter    string      `json:"meter"    yaml:"meter"`
	Amount   int64       `json:"amount"   yaml:"amount"`
	Window   interface{} `json:"window"   yaml:"window"` // string ("30s") or int64 ns
	Strategy string      `json:"strategy" yaml:"strategy"`
}

// RelayKeyDTO is the wire form of a RelayKey. Policy is a name.
type RelayKeyDTO struct {
	APIVersion string       `json:"apiVersion" yaml:"apiVersion"`
	Kind       string       `json:"kind"       yaml:"kind"`
	Metadata   WireMeta     `json:"metadata"   yaml:"metadata"`
	Spec       RelayKeySpec `json:"spec"       yaml:"spec"`
}

type RelayKeySpec struct {
	// Policy is the policy *name* (wire form).
	Policy                string  `json:"policy"                      yaml:"policy"`
	KeyHash               string  `json:"keyHash"                     yaml:"keyHash"`
	Prefix                string  `json:"prefix,omitempty"            yaml:"prefix,omitempty"`
	RevokedAt             *string `json:"revokedAt,omitempty"         yaml:"revokedAt,omitempty"`
	Enabled               *bool   `json:"enabled,omitempty"           yaml:"enabled,omitempty"`
	PassthroughAllowed    bool    `json:"passthroughAllowed,omitempty" yaml:"passthroughAllowed,omitempty"`
	PayloadLoggingEnabled bool    `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty"`
}

// PricingDTO is the wire form of a Pricing. Owner.ID is a host *name* here.
// TargetModels holds model *names* (wire form).
type PricingDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       PricingSpec `json:"spec"       yaml:"spec"`
}

type PricingSpec struct {
	Currency     string           `json:"currency"          yaml:"currency"`
	TargetModels []string         `json:"targetModels"      yaml:"targetModels"`
	Rates        []PricingRateDTO `json:"rates"             yaml:"rates"`
	Enabled      *bool            `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// PricingRateDTO mirrors pricing.Rate using plain types.
type PricingRateDTO struct {
	Meter       string  `json:"meter"                 yaml:"meter"`
	Unit        string  `json:"unit"                  yaml:"unit"`
	Amount      float64 `json:"amount"                yaml:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty" yaml:"aboveTokens,omitempty"`
}

// SettingDTO is the wire form of a settings section. Unlike the catalog kinds
// the spec shape is not fixed — it varies per section, with metadata.name
// selecting the registered settings.Section whose typed value the spec must
// match. The spec is therefore carried as a raw node and validated downstream
// by that section's Decode. Settings are singletons keyed by name, so the
// owner/id/label metadata fields are unused.
type SettingDTO struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind"       yaml:"kind"`
	Metadata   WireMeta  `json:"metadata"   yaml:"metadata"`
	Spec       yaml.Node `json:"-"          yaml:"spec"`
}

// SpecJSON renders the raw spec node as JSON for the settings store, which
// validates it against the section's typed value via the section's Decode.
func (d *SettingDTO) SpecJSON() (json.RawMessage, error) {
	var v any
	if err := d.Spec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

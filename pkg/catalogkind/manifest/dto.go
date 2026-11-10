package manifest

import (
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/meta"
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
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// WireOwner is the wire form of meta.Owner. The referenced row is named —
// translate functions resolve Name → id when producing the domain shape,
// and reverse-resolve id → Name when emitting the wire shape. ID is the
// id-form for API clients that already hold a UUID; either field is
// accepted on read, with ID taking precedence.
type WireOwner struct {
	Kind meta.OwnerKind `json:"kind,omitempty" yaml:"kind,omitempty" enum:"system,user,team,project,provider,host"`
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

// ToMeta converts the wire metadata to the domain shape; the owner keeps the name-or-id the document gave, for the caller to resolve.
func (w WireMeta) ToMeta() meta.Metadata {
	return meta.Metadata{
		ID:          w.ID,
		Name:        w.Name,
		DisplayName: w.DisplayName,
		Description: w.Description,
		Owner:       meta.Owner{Kind: w.Owner.Kind, ID: w.Owner.ref()},
		Labels:      w.Labels,
		Annotations: w.Annotations,
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
	BaseURL string `json:"baseURL" yaml:"baseURL"`
	// Path overrides the adapter's shape-default upstream path. Verbatim when
	// set; explicit "" appends nothing; absent keeps the shape default.
	Path    *string           `json:"path,omitempty"        yaml:"path,omitempty"`
	Backend map[string]string `json:"backend,omitempty"     yaml:"backend,omitempty"`
	NoAuth  bool              `json:"noAuth,omitempty"      yaml:"noAuth,omitempty"`
	// PricingStrategies is the host's menu of offered billing modes
	// (api | sub). Empty defaults to ["api"].
	PricingStrategies []string   `json:"pricingStrategies,omitempty" yaml:"pricingStrategies,omitempty"`
	Enabled           *bool      `json:"enabled,omitempty"     yaml:"enabled,omitempty"`
	HomepageURL       string     `json:"homepageURL,omitempty" yaml:"homepageURL,omitempty"`
	DocsURL           string     `json:"docsURL,omitempty"     yaml:"docsURL,omitempty"`
	ConsoleURL        string     `json:"consoleURL,omitempty"  yaml:"consoleURL,omitempty"`
	StatusPageURL     string     `json:"statusPageURL,omitempty" yaml:"statusPageURL,omitempty"`
	Icon              *meta.Icon `json:"icon,omitempty"        yaml:"icon,omitempty"`
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
	Family      string `json:"family,omitempty"      yaml:"family,omitempty"`
	ProductLine string `json:"productLine,omitempty" yaml:"productLine,omitempty"`
	Version     string `json:"version,omitempty"     yaml:"version,omitempty"`

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

	// Aliases are resolution-only matchers (see model.Spec.Aliases). Plain
	// strings, no cross-refs to resolve.
	Aliases []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
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
	ServiceTier string  `json:"serviceTier,omitempty" yaml:"serviceTier,omitempty"`
}

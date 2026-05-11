package catalog

import (
	"encoding/json"
	"fmt"
	"time"
)

const APIVersion = "relay.wyolet.dev/v1"

type Kind string

const (
	KindProvider  Kind = "Provider"
	KindModel     Kind = "Model"
	KindRoute     Kind = "Route"
	KindRateLimit Kind = "RateLimit"
	KindSecret    Kind = "Secret"
	KindPolicy      Kind = "Policy"
	KindRelayKey    Kind = "RelayKey"
	KindPassthrough Kind = "Passthrough"
)

// PassthroughSingletonName is the canonical Metadata.Name of the singleton
// Passthrough resource. There is one Passthrough config per Relay instance.
const PassthroughSingletonName = "default"

type ProviderKind string

const (
	PKOllama    ProviderKind = "ollama"
	PKOpenAI    ProviderKind = "openai"
	PKAnthropic ProviderKind = "anthropic"
)

type Metadata struct {
	Name   string            `yaml:"name"   json:"name"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type Provider struct {
	APIVersion string       `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind         `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata     `yaml:"metadata"   json:"metadata"`
	Spec       ProviderSpec `yaml:"spec"       json:"spec"`
}

// IsEnabled returns true when an Enabled flag is unset (nil) or explicitly true.
// Default is on; the frontend toggles to false to disable.
func IsEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

type ProviderSpec struct {
	Kind        ProviderKind `yaml:"kind"        json:"kind"`
	BaseURL     string       `yaml:"baseURL"     json:"baseURL"`
	Default     bool         `yaml:"default,omitempty"     json:"default,omitempty"`
	DefaultPolicy string       `yaml:"defaultPolicy,omitempty" json:"defaultPolicy,omitempty"`

	// Enabled defaults to true when nil. False disables the resource at the
	// data plane (filtered from routing/keypool selection); admin reads still
	// see it so the UI can toggle it back on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// Display metadata — operator-set, optional.
	DisplayName   string `yaml:"displayName,omitempty"   json:"displayName,omitempty"`
	Description   string `yaml:"description,omitempty"   json:"description,omitempty"`
	HomepageURL   string `yaml:"homepageURL,omitempty"   json:"homepageURL,omitempty"`
	DocsURL       string `yaml:"docsURL,omitempty"       json:"docsURL,omitempty"`
	ConsoleURL    string `yaml:"consoleURL,omitempty"    json:"consoleURL,omitempty"`
	StatusPageURL string `yaml:"statusPageURL,omitempty" json:"statusPageURL,omitempty"`
	LogoURL       string `yaml:"logoURL,omitempty"       json:"logoURL,omitempty"`

	// DefaultPricing is merged with Model.Spec.Pricing at snapshot load.
	// Model-level rates win on collision per-key.
	DefaultPricing *Pricing `yaml:"defaultPricing,omitempty" json:"defaultPricing,omitempty"`
}

type Secret struct {
	APIVersion  string     `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind        Kind       `yaml:"kind"       json:"kind,omitempty"`
	Metadata    Metadata   `yaml:"metadata"   json:"metadata"`
	Spec        SecretSpec `yaml:"spec"       json:"spec"`
	Resolved    string     `json:"-"`
	KeyHash     string     `json:"-"`
	UsedLiteral bool       `json:"-"`
}

type SecretSpec struct {
	Provider   string                `yaml:"provider"             json:"provider,omitempty"`
	ValueFrom  *SecretValueFrom      `yaml:"valueFrom,omitempty"  json:"valueFrom,omitempty"`
	Value      string                `yaml:"value,omitempty"      json:"-"` // never serialised to JSON (cleartext)
	RateLimits []RateLimitAttachment `yaml:"rateLimits,omitempty" json:"rateLimits,omitempty"`
	Enabled    *bool                 `yaml:"enabled,omitempty"    json:"enabled,omitempty"`
}

type SecretValueFrom struct {
	Env string `yaml:"env" json:"env,omitempty"`
}

type Policy struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind     `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata `yaml:"metadata"   json:"metadata"`
	Spec       PolicySpec `yaml:"spec"       json:"spec"`
}

type PolicySpec struct {
	Provider          string                `yaml:"provider"              json:"provider"`
	Secrets           []string              `yaml:"secrets,omitempty"     json:"secrets,omitempty"`
	SecretSelector    map[string]string     `yaml:"secretSelector,omitempty" json:"secretSelector,omitempty"`
	// Models is the allowed-list of model names callable through this policy.
	// Empty/nil = any model registered for the policy's provider.
	// Populated = only listed model names allowed; unknown model rejected
	// at dispatch time with model_not_allowed.
	Models            []string              `yaml:"models,omitempty"      json:"models,omitempty"`
	RateLimits        []RateLimitAttachment `yaml:"rateLimits,omitempty"  json:"rateLimits,omitempty"`
	SkipDefaultLimits bool                  `yaml:"skipDefaultLimits,omitempty" json:"skipDefaultLimits,omitempty"`
	// Passthrough, when true, means the policy has no relay-managed keys.
	// The inbound Authorization header is forwarded verbatim to upstream.
	// Secrets and SecretSelector must be empty.
	Passthrough bool  `yaml:"passthrough,omitempty" json:"passthrough,omitempty"`
	Enabled     *bool `yaml:"enabled,omitempty"     json:"enabled,omitempty"`
}

type Model struct {
	APIVersion string    `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind      `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata  `yaml:"metadata"   json:"metadata"`
	Spec       ModelSpec `yaml:"spec"       json:"spec"`
}

type ModelSpec struct {
	Provider     string `yaml:"provider"     json:"provider"`
	UpstreamName string `yaml:"upstreamName" json:"upstreamName"`

	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Family      string `yaml:"family,omitempty"      json:"family,omitempty"`
	Version     string `yaml:"version,omitempty"     json:"version,omitempty"`

	Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Modalities   Modalities   `yaml:"modalities,omitempty"   json:"modalities,omitempty"`

	// ContextWindow is the legacy field (alias for ContextWindowTotal). Kept for backward compat.
	ContextWindow   int `yaml:"contextWindow,omitempty"   json:"contextWindow,omitempty"`
	MaxOutputTokens int `yaml:"maxOutputTokens,omitempty" json:"maxOutputTokens,omitempty"`

	// Context window split. ContextWindow continues to parse as ContextWindowTotal alias.
	ContextWindowInput  int `yaml:"contextWindowInput,omitempty"  json:"contextWindowInput,omitempty"`
	ContextWindowOutput int `yaml:"contextWindowOutput,omitempty" json:"contextWindowOutput,omitempty"`
	ContextWindowTotal  int `yaml:"contextWindowTotal,omitempty"  json:"contextWindowTotal,omitempty"`

	Pricing *Pricing `yaml:"pricing,omitempty" json:"pricing,omitempty"`

	KnowledgeCutoff string      `yaml:"knowledgeCutoff,omitempty" json:"knowledgeCutoff,omitempty"`
	ReleaseDate     string      `yaml:"releaseDate,omitempty"     json:"releaseDate,omitempty"`
	DeprecationDate string      `yaml:"deprecationDate,omitempty" json:"deprecationDate,omitempty"`
	Deprecation     *Deprecation `yaml:"deprecation,omitempty"    json:"deprecation,omitempty"`

	Documentation string `yaml:"documentation,omitempty" json:"documentation,omitempty"`
	License       string `yaml:"license,omitempty"       json:"license,omitempty"`

	// Display metadata.
	DisplayName          string   `yaml:"displayName,omitempty"          json:"displayName,omitempty"`
	Aliases              []string `yaml:"aliases,omitempty"              json:"aliases,omitempty"`
	Tags                 []string `yaml:"tags,omitempty"                 json:"tags,omitempty"`
	ProviderModelPageURL string   `yaml:"providerModelPageURL,omitempty" json:"providerModelPageURL,omitempty"`

	RateLimits []RateLimitAttachment `yaml:"rateLimits,omitempty" json:"rateLimits,omitempty"`

	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// Deprecation describes the lifecycle state of a model.
type Deprecation struct {
	Status      string `yaml:"status,omitempty"      json:"status,omitempty"` // active | deprecated | sunset
	SunsetDate  string `yaml:"sunsetDate,omitempty"  json:"sunsetDate,omitempty"`
	Replacement string `yaml:"replacement,omitempty" json:"replacement,omitempty"`
}

type Capabilities struct {
	Chat             bool `yaml:"chat,omitempty"             json:"chat,omitempty"`
	Embeddings       bool `yaml:"embeddings,omitempty"       json:"embeddings,omitempty"`
	Streaming        bool `yaml:"streaming,omitempty"        json:"streaming,omitempty"`
	Tools            bool `yaml:"tools,omitempty"            json:"tools,omitempty"`
	ParallelTools    bool `yaml:"parallelTools,omitempty"    json:"parallelTools,omitempty"`
	Vision           bool `yaml:"vision,omitempty"           json:"vision,omitempty"`
	Audio            bool `yaml:"audio,omitempty"            json:"audio,omitempty"`
	PromptCache      bool `yaml:"promptCache,omitempty"      json:"promptCache,omitempty"`
	Reasoning        bool `yaml:"reasoning,omitempty"        json:"reasoning,omitempty"`
	JSONMode         bool `yaml:"jsonMode,omitempty"         json:"jsonMode,omitempty"`
	StructuredOutput bool `yaml:"structuredOutput,omitempty" json:"structuredOutput,omitempty"`
	// StructuredOutputs is a preferred alias for StructuredOutput; both are accepted.
	StructuredOutputs bool `yaml:"structuredOutputs,omitempty" json:"structuredOutputs,omitempty"`
	Batch             bool `yaml:"batch,omitempty"             json:"batch,omitempty"`
	ComputerUse       bool `yaml:"computerUse,omitempty"       json:"computerUse,omitempty"`
	WebSearch         bool `yaml:"webSearch,omitempty"         json:"webSearch,omitempty"`
	FileInput         bool `yaml:"fileInput,omitempty"         json:"fileInput,omitempty"`
	AudioInput        bool `yaml:"audioInput,omitempty"        json:"audioInput,omitempty"`
	AudioOutput       bool `yaml:"audioOutput,omitempty"       json:"audioOutput,omitempty"`
	SystemMessages    bool `yaml:"systemMessages,omitempty"    json:"systemMessages,omitempty"`
	AssistantPrefill  bool `yaml:"assistantPrefill,omitempty"  json:"assistantPrefill,omitempty"`
}

type Modalities struct {
	Input  []string `yaml:"input,omitempty"  json:"input,omitempty"`
	Output []string `yaml:"output,omitempty" json:"output,omitempty"`
}

// Pricing describes per-unit costs for a model or provider default.
// Keys in Rates are open-ended meter names (e.g. "tokens.input", "tokens.output",
// "tokens.cache_read", "requests", "images").
type Pricing struct {
	Currency string             `yaml:"currency"           json:"currency"` // ISO 4217 code e.g. "USD"
	Unit     PricingUnit        `yaml:"unit"               json:"unit"`     // per_million | per_thousand | per_unit
	Rates    map[string]float64 `yaml:"rates,omitempty"    json:"rates,omitempty"`
}

// PricingUnit is the denominator for rates.
type PricingUnit string

const (
	PricingUnitPerMillion  PricingUnit = "per_million"
	PricingUnitPerThousand PricingUnit = "per_thousand"
	PricingUnitPerUnit     PricingUnit = "per_unit"
)

type Route struct {
	APIVersion string    `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind      `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata  `yaml:"metadata"   json:"metadata"`
	Spec       RouteSpec `yaml:"spec"       json:"spec"`
}

type RouteSpec struct {
	Default bool     `yaml:"default,omitempty" json:"default,omitempty"`
	Models  []string `yaml:"models"            json:"models"`
	Enabled *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

type RateLimit struct {
	APIVersion string        `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind          `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata      `yaml:"metadata"   json:"metadata"`
	Spec       RateLimitSpec `yaml:"spec"       json:"spec"`
}

// RateLimitRule is one meter/amount pair within a RateLimitSpec.
type RateLimitRule struct {
	Meter  string `yaml:"meter"            json:"meter"`
	Amount int64  `yaml:"amount"           json:"amount"`
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
}

// RateLimitSpec is the canonical multi-rule shape. Top-level amount/meter/source
// were retired (see issue #78); legacy inputs are silently lifted into a
// one-element Rules list by UnmarshalJSON / UnmarshalYAML for backward compat
// with stored JSONB rows and YAML fixtures.
type RateLimitSpec struct {
	Strategy RateLimitStrategy `yaml:"strategy" json:"strategy"`
	Window   time.Duration     `yaml:"window"   json:"window"`
	Rules    []RateLimitRule   `yaml:"rules"    json:"rules"`
	Enabled  *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// rateLimitSpecJSON is the legacy-tolerant unmarshal shim. It accepts either
// the canonical {strategy, window, rules[], enabled?} shape or the legacy
// {strategy, window, amount, meter?, source?, enabled?} shape and lifts the
// latter into a single-element rules list.
type rateLimitSpecJSON struct {
	Strategy RateLimitStrategy `json:"strategy"`
	Window   time.Duration     `json:"window"`
	Rules    []RateLimitRule   `json:"rules,omitempty"`
	Amount   int64             `json:"amount,omitempty"`
	Meter    string            `json:"meter,omitempty"`
	Source   string            `json:"source,omitempty"`
	Enabled  *bool             `json:"enabled,omitempty"`
}

func (s *RateLimitSpec) UnmarshalJSON(b []byte) error {
	var raw rateLimitSpecJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Strategy = raw.Strategy
	s.Window = raw.Window
	s.Enabled = raw.Enabled
	s.Rules = raw.Rules
	if len(s.Rules) == 0 && (raw.Amount != 0 || raw.Meter != "" || raw.Source != "") {
		meter := raw.Meter
		if meter == "" {
			meter = string(MeterRequests)
		}
		s.Rules = []RateLimitRule{{Meter: meter, Amount: raw.Amount, Source: raw.Source}}
	}
	return nil
}

func (s *RateLimitSpec) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Strategy RateLimitStrategy `yaml:"strategy"`
		Window   time.Duration     `yaml:"window"`
		Rules    []RateLimitRule   `yaml:"rules,omitempty"`
		Amount   int64             `yaml:"amount,omitempty"`
		Meter    string            `yaml:"meter,omitempty"`
		Source   string            `yaml:"source,omitempty"`
		Enabled  *bool             `yaml:"enabled,omitempty"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	s.Strategy = raw.Strategy
	s.Window = raw.Window
	s.Enabled = raw.Enabled
	s.Rules = raw.Rules
	if len(s.Rules) == 0 && (raw.Amount != 0 || raw.Meter != "" || raw.Source != "") {
		meter := raw.Meter
		if meter == "" {
			meter = string(MeterRequests)
		}
		s.Rules = []RateLimitRule{{Meter: meter, Amount: raw.Amount, Source: raw.Source}}
	}
	return nil
}

// NormalizedRules returns the effective rule list. Retained as a stable
// accessor; equivalent to s.Rules now that the spec is canonical.
func (s *RateLimitSpec) NormalizedRules() []RateLimitRule {
	return s.Rules
}

type RateLimitStrategy string

const (
	StrategySlidingWindow RateLimitStrategy = "sliding-window"
)

type RateLimitSource string

const (
	SourceUserDefined    RateLimitSource = "user_defined"
	SourceSystemMirrored RateLimitSource = "system_mirrored"
)

// RateLimitAttachment is a reference by name from a Policy/Secret/Model to a RateLimit.
// It accepts two YAML/JSON shapes for backward compatibility:
//
//	New:    "my-rate-limit"                 (plain string)
//	Legacy: {ref: "my-rate-limit", meter: "requests"}  (the meter field is now ignored)
type RateLimitAttachment struct {
	Ref string // the RateLimit name
}

func (a RateLimitAttachment) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.Ref)
}

func (a *RateLimitAttachment) UnmarshalJSON(b []byte) error {
	// Try plain string first.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		a.Ref = s
		return nil
	}
	// Fall back to legacy {ref, meter} object (meter is silently ignored).
	var legacy struct {
		Ref   string `json:"ref"`
		Meter string `json:"meter"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil {
		return fmt.Errorf("RateLimitAttachment: expected string or {ref,meter} object: %w", err)
	}
	if legacy.Ref == "" {
		return fmt.Errorf("RateLimitAttachment: ref must not be empty")
	}
	a.Ref = legacy.Ref
	return nil
}

func (a RateLimitAttachment) MarshalYAML() (any, error) {
	return a.Ref, nil
}

func (a *RateLimitAttachment) UnmarshalYAML(unmarshal func(any) error) error {
	// Try plain string.
	var s string
	if err := unmarshal(&s); err == nil {
		a.Ref = s
		return nil
	}
	// Fall back to legacy {ref, meter} object.
	var legacy struct {
		Ref   string `yaml:"ref"`
		Meter string `yaml:"meter"`
	}
	if err := unmarshal(&legacy); err != nil {
		return fmt.Errorf("RateLimitAttachment: expected string or {ref, meter} mapping: %w", err)
	}
	if legacy.Ref == "" {
		return fmt.Errorf("RateLimitAttachment: ref must not be empty")
	}
	a.Ref = legacy.Ref
	return nil
}

type Meter string

const (
	MeterRequests    Meter = "requests"
	MeterTokens      Meter = "tokens"
	MeterConcurrency Meter = "concurrency"
)

// ResolvedRule is one concrete rule derived from attaching a RateLimit.
// A single RateLimit with N rules produces N ResolvedRule entries.
type ResolvedRule struct {
	ParentKind    Kind
	ParentName    string
	RateLimitName string // name of the parent RateLimit object
	Strategy      RateLimitStrategy
	Window        time.Duration
	Rule          RateLimitRule // the specific rule from the RateLimit

	// RateLimit is kept for backward compat with code that still reads the
	// full RateLimit object (keys.go, scripts.go). Updated by snapshot.go.
	RateLimit *RateLimit

	// Meter is the rule's meter as a catalog.Meter for legacy callers.
	// Equal to Meter(Rule.Meter). Updated by snapshot.go.
	Meter Meter
}

// RelayKey is a relay-managed API key. Plaintext is never stored; only the
// sha256 hex of the bearer token (KeyHash) is persisted, plus a short Prefix
// for UI display. PolicyRef optionally binds the key to a Policy whose
// allowed-models list and rate limits will apply to requests authenticated
// with this key (overriding the provider's defaultPolicy).
type RelayKey struct {
	APIVersion string       `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind         `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata     `yaml:"metadata"   json:"metadata"`
	Spec       RelayKeySpec `yaml:"spec"       json:"spec"`
}

type RelayKeySpec struct {
	// KeyHash is the sha256 hex of the bearer token (lowercase, 64 chars).
	// Required and immutable after creation.
	KeyHash string `yaml:"keyHash" json:"keyHash"`
	// Prefix is the leading visible portion of the token (e.g. "rk_a8b3f2"),
	// retained so the UI can display a recognisable identifier without storing
	// the cleartext.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// PolicyRef binds this key to a named Policy. Optional; when set, dispatch
	// uses the referenced Policy's allowed-models and rate limits in place of
	// the provider's defaultPolicy.
	PolicyRef string `yaml:"policyRef,omitempty" json:"policyRef,omitempty"`
	// RevokedAt, when set, means the key is rejected at auth time. Use the
	// restore endpoint to clear it.
	RevokedAt *time.Time `yaml:"revokedAt,omitempty" json:"revokedAt,omitempty"`
	// Enabled defaults to true when nil. False disables auth for this key.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// Passthrough is the singleton config that controls BYO-credential request
// handling: whether the relay forwards the inbound Authorization header
// verbatim to upstream instead of selecting from relay-managed secrets.
//
// One Passthrough per relay instance, addressable as
// /control/passthrough. The resource is k8s-shaped for consistency with
// other catalog kinds; the singleton invariant is enforced by validation
// (Metadata.Name must equal PassthroughSingletonName).
type Passthrough struct {
	APIVersion string          `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind            `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata        `yaml:"metadata"   json:"metadata"`
	Spec       PassthroughSpec `yaml:"spec"       json:"spec"`
}

type PassthroughSpec struct {
	// Enabled is the master switch. When false, all passthrough behaviour is
	// off regardless of the nested toggles.
	Enabled bool `yaml:"enabled" json:"enabled"`

	Unauthenticated PassthroughUnauthenticated `yaml:"unauthenticated" json:"unauthenticated"`
	Models          PassthroughModels          `yaml:"models"          json:"models"`

	// Transports lists which protocols accept passthrough requests. Today only
	// "http" is wired in the data plane; the others are reserved.
	Transports []string `yaml:"transports,omitempty" json:"transports,omitempty"`
}

type PassthroughUnauthenticated struct {
	// Enabled allows passthrough requests with no Relay key (raw upstream
	// Authorization only). When false, callers must present a valid Relay key
	// (X-WR-API-Key) marked passthroughAllowed=true.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// BucketBy declares how unauthenticated callers are aggregated for rate
	// limiting and accounting. Required when Enabled=true.
	BucketBy PassthroughBucketBy `yaml:"bucketBy,omitempty" json:"bucketBy,omitempty"`
}

type PassthroughBucketBy string

const (
	PassthroughBucketByCredentialHash PassthroughBucketBy = "credential_hash"
)

type PassthroughModels struct {
	// Mode selects between unrestricted ("all") and an explicit allow-list
	// ("allowlist"). Empty array semantics ("[] = all") are intentionally not
	// supported — the wire shape says what it means.
	Mode PassthroughModelsMode `yaml:"mode" json:"mode"`
	// Allow is the list of model names callable via passthrough when
	// Mode="allowlist". Required (non-empty) in that mode; ignored otherwise.
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
}

type PassthroughModelsMode string

const (
	PassthroughModelsModeAll       PassthroughModelsMode = "all"
	PassthroughModelsModeAllowlist PassthroughModelsMode = "allowlist"
)

// DefaultPassthrough is what GET /control/passthrough returns when no row
// has been written. Disabled by default; safe-by-default posture.
func DefaultPassthrough() *Passthrough {
	return &Passthrough{
		APIVersion: APIVersion,
		Kind:       KindPassthrough,
		Metadata:   Metadata{Name: PassthroughSingletonName},
		Spec: PassthroughSpec{
			Enabled:         false,
			Unauthenticated: PassthroughUnauthenticated{Enabled: false},
			Models:          PassthroughModels{Mode: PassthroughModelsModeAll},
			Transports:      []string{"http"},
		},
	}
}

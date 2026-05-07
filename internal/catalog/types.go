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
	KindPool      Kind = "Pool"
)

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

type ProviderSpec struct {
	Kind        ProviderKind `yaml:"kind"        json:"kind"`
	BaseURL     string       `yaml:"baseURL"     json:"baseURL"`
	Default     bool         `yaml:"default,omitempty"     json:"default,omitempty"`
	DefaultPool string       `yaml:"defaultPool,omitempty" json:"defaultPool,omitempty"`

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
}

type SecretValueFrom struct {
	Env string `yaml:"env" json:"env,omitempty"`
}

type Pool struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion,omitempty"`
	Kind       Kind     `yaml:"kind"       json:"kind,omitempty"`
	Metadata   Metadata `yaml:"metadata"   json:"metadata"`
	Spec       PoolSpec `yaml:"spec"       json:"spec"`
}

type PoolSpec struct {
	Provider          string                `yaml:"provider"              json:"provider"`
	Secrets           []string              `yaml:"secrets,omitempty"     json:"secrets,omitempty"`
	SecretSelector    map[string]string     `yaml:"secretSelector,omitempty" json:"secretSelector,omitempty"`
	RateLimits        []RateLimitAttachment `yaml:"rateLimits,omitempty"  json:"rateLimits,omitempty"`
	SkipDefaultLimits bool                  `yaml:"skipDefaultLimits,omitempty" json:"skipDefaultLimits,omitempty"`
	// Passthrough, when true, means the pool has no relay-managed keys.
	// The inbound Authorization header is forwarded verbatim to upstream.
	// Secrets and SecretSelector must be empty.
	Passthrough bool `yaml:"passthrough,omitempty" json:"passthrough,omitempty"`
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

type RateLimitSpec struct {
	Strategy RateLimitStrategy `yaml:"strategy" json:"strategy"`
	Window   time.Duration     `yaml:"window"   json:"window"`

	// Rules is the multi-rule list. If non-empty it is the canonical shape.
	Rules []RateLimitRule `yaml:"rules,omitempty" json:"rules,omitempty"`

	// Legacy single-rule fields. Still accepted on input; lifted into Rules
	// by NormalizedRules(). Marshalled only when Rules is empty.
	Amount int64           `yaml:"amount,omitempty" json:"amount,omitempty"`
	Meter  string          `yaml:"meter,omitempty"  json:"meter,omitempty"`
	Source RateLimitSource `yaml:"source,omitempty" json:"source,omitempty"`
}

// NormalizedRules returns the effective rule list for a spec.
// If Rules is non-empty it is returned unchanged. Otherwise the legacy
// Amount/Meter/Source fields are wrapped in a one-element list so all
// downstream code paths are uniform. Read-only — never mutates the spec.
func (s *RateLimitSpec) NormalizedRules() []RateLimitRule {
	if len(s.Rules) > 0 {
		return s.Rules
	}
	meter := s.Meter
	if meter == "" {
		meter = string(MeterRequests)
	}
	return []RateLimitRule{{
		Meter:  meter,
		Amount: s.Amount,
		Source: string(s.Source),
	}}
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

// RateLimitAttachment is a reference by name from a Pool/Secret/Model to a RateLimit.
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

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

// OwnerKind identifies the provenance of a catalog resource.
type OwnerKind string

const (
	OwnerSystem   OwnerKind = "system"
	OwnerProvider OwnerKind = "provider"
	OwnerUser     OwnerKind = "user"
)

// Owner describes who created / manages a catalog resource.
// Kind=provider requires ID (the provider UUID or name slug); the other kinds
// leave ID empty.
type Owner struct {
	Kind OwnerKind `yaml:"kind" json:"kind"`
	ID   string    `yaml:"id,omitempty" json:"id,omitempty"`
}

// Metadata is the identity tuple for every catalog resource.
//
//   - ID is the immutable primary key (UUIDv7), server-stamped on create.
//     PG joins and JSONB cross-references store this value.
//   - Name is a stable, DNS-1123 slug. Auto-derived from DisplayName on create
//     with a collision suffix; editable via id-routed PUT. URLs and YAML refs
//     surface this; never used for cross-references in stored JSONB.
//   - DisplayName is free text shown in UI. Editing it is cheap and never
//     touches references.
//   - Description is free text documenting the resource. Moved here from spec
//     on kinds that previously carried it there (RateLimit, Provider).
//   - Owner identifies the provenance of the resource. Defaults: system for
//     Provider/UpstreamTier; user for Secret/RelayKey/Policy/Route;
//     provider (with ID) for Model; mixed for RateLimit.
type Metadata struct {
	ID          string            `yaml:"id,omitempty"          json:"id,omitempty"`
	Name        string            `yaml:"name"                  json:"name"`
	DisplayName string            `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Owner       Owner             `yaml:"owner,omitempty"       json:"owner,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"      json:"labels,omitempty"`
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
	// (DisplayName lives on Metadata; do not duplicate here.)
	Description   string `yaml:"description,omitempty"   json:"description,omitempty"`
	HomepageURL   string `yaml:"homepageURL,omitempty"   json:"homepageURL,omitempty"`
	DocsURL       string `yaml:"docsURL,omitempty"       json:"docsURL,omitempty"`
	ConsoleURL    string `yaml:"consoleURL,omitempty"    json:"consoleURL,omitempty"`
	StatusPageURL string `yaml:"statusPageURL,omitempty" json:"statusPageURL,omitempty"`
	LogoURL       string `yaml:"logoURL,omitempty"       json:"logoURL,omitempty"`

	// DefaultPricing is merged with Model.Spec.Pricing at snapshot load.
	// Model-level rates win on collision per-key.
	DefaultPricing *Pricing `yaml:"defaultPricing,omitempty" json:"defaultPricing,omitempty"`

	// DefaultTier is the upstream billing tier for keys that don't declare one.
	// When set, the snapshot loader auto-injects a system_mirrored RateLimit
	// named "upstream-<secret>-<tier>" for every secret on this provider that
	// does not override Tier itself. Must be a value from AllUpstreamTiers or empty.
	DefaultTier string `yaml:"defaultTier,omitempty" json:"defaultTier,omitempty" enum:"openai-tier-1,openai-tier-2,openai-tier-3,openai-tier-4,openai-tier-5,anthropic-tier-1,anthropic-tier-2,anthropic-tier-3,anthropic-tier-4" doc:"Default upstream billing tier for secrets on this provider. Auto-injects a system_mirrored RateLimit when set."`
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

	// Tier is the upstream billing tier for this specific key. When set, the
	// snapshot loader auto-injects a system_mirrored RateLimit named
	// "upstream-<secret>-<tier>". Overrides Provider.Spec.DefaultTier.
	// Must be a value from AllUpstreamTiers or empty.
	Tier string `yaml:"tier,omitempty" json:"tier,omitempty" enum:"openai-tier-1,openai-tier-2,openai-tier-3,openai-tier-4,openai-tier-5,anthropic-tier-1,anthropic-tier-2,anthropic-tier-3,anthropic-tier-4" doc:"Upstream billing tier for this key. Overrides provider.spec.defaultTier. Auto-injects a system_mirrored RateLimit when set."`
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

// KeySelection controls how the key pool selects a secret from the healthy candidates.
type KeySelection string

const (
	// KeySelectionPrioritized drains keys in declaration order: the first healthy
	// key in the policy's secrets list is always preferred. Default — matches the
	// most common operator request ("drain key 1 fully before key 2").
	KeySelectionPrioritized KeySelection = "prioritized"
	// KeySelectionRoundRobin distributes traffic evenly across healthy keys in a
	// rotating order.
	KeySelectionRoundRobin KeySelection = "round-robin"
	// KeySelectionLeastRecentlyUsed prefers the key that was least recently used,
	// spreading load based on recency rather than a counter.
	KeySelectionLeastRecentlyUsed KeySelection = "least-recently-used"
)

// AllKeySelections enumerates every valid KeySelection value.
var AllKeySelections = []KeySelection{
	KeySelectionPrioritized,
	KeySelectionRoundRobin,
	KeySelectionLeastRecentlyUsed,
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
	Enabled           *bool                 `yaml:"enabled,omitempty"     json:"enabled,omitempty"`
	// KeySelection controls the strategy used to pick a secret from the healthy pool.
	// Valid values: "prioritized" (default), "round-robin", "least-recently-used".
	KeySelection      KeySelection          `yaml:"keySelection,omitempty" json:"keySelection,omitempty" enum:"prioritized,round-robin,least-recently-used" doc:"Key selection strategy for this policy's key pool. Defaults to 'prioritized' — drain first healthy key in declaration order."`
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

	// Display metadata. (DisplayName lives on Metadata.)
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

// RateLimitRule is one meter/amount/window/strategy tuple within a RateLimitSpec.
// Every rule MUST declare its own window and strategy — there is no spec-level
// default fallback. Concurrency meter ignores strategy.
type RateLimitRule struct {
	Meter    string            `yaml:"meter"    json:"meter"    enum:"requests,concurrency,tokens,tokens.input,tokens.output,tokens.cache_read,tokens.cache_creation,tokens.reasoning,tokens.server_tool_use_input,tokens.server_tool_use_output" doc:"Meter to count against. Bare 'tokens' sums every sub-meter; tokens.<key> targets one."`
	Amount   int64             `yaml:"amount"   json:"amount"`
	Window   time.Duration     `yaml:"window"   json:"window"   doc:"Rule window (nanoseconds, or human-readable string on input — '30s', '1m'). Required."` // serialized via the ruleJSON shim; this tag exists so huma includes the field in the OpenAPI schema
	Strategy RateLimitStrategy `yaml:"strategy" json:"strategy" enum:"token-bucket,sliding-window,fixed-window,leaky-bucket,session-window" doc:"Rate-limit strategy. Required."`
}

// rateLimitRuleJSON is the JSON unmarshal shim for RateLimitRule, accepting
// window as a human-readable string or nanosecond integer.
type rateLimitRuleJSON struct {
	Meter    string            `json:"meter"`
	Amount   int64             `json:"amount"`
	Window   jsonDuration      `json:"window,omitempty"`
	Strategy RateLimitStrategy `json:"strategy,omitempty"`
}

func (r *RateLimitRule) UnmarshalJSON(b []byte) error {
	var raw rateLimitRuleJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	r.Meter = raw.Meter
	r.Amount = raw.Amount
	r.Window = time.Duration(raw.Window)
	r.Strategy = raw.Strategy
	return nil
}

func (r RateLimitRule) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Meter    string            `json:"meter"`
		Amount   int64             `json:"amount"`
		Window   int64             `json:"window,omitempty"`
		Strategy RateLimitStrategy `json:"strategy,omitempty"`
	}
	return json.Marshal(Alias{
		Meter:    r.Meter,
		Amount:   r.Amount,
		Window:   int64(r.Window),
		Strategy: r.Strategy,
	})
}

// RateLimitSpec is the canonical multi-rule shape. Every rule MUST declare its
// own window and strategy — spec-level defaults (window, strategy) are gone.
// Source/description were moved to Metadata in issue #105.
type RateLimitSpec struct {
	Rules   []RateLimitRule `yaml:"rules"            json:"rules"`
	Enabled *bool           `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// jsonDuration is a time.Duration that accepts both nanosecond integers and
// human-readable strings ("1m", "30s") in JSON. Storage writes integers (Go
// default); API callers often send strings.
type jsonDuration time.Duration

func (d *jsonDuration) UnmarshalJSON(b []byte) error {
	// Try string first ("1m", "30s", etc.)
	if len(b) > 1 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = jsonDuration(v)
		return nil
	}
	// Fall back to nanoseconds-as-integer (legacy / storage format).
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*d = jsonDuration(n)
	return nil
}

// rateLimitSpecJSON is the unmarshal shim for RateLimitSpec. It accepts the
// canonical {rules[], enabled?} shape. Legacy fields (strategy, window, source,
// description, amount, meter) are silently absorbed for backward-compat with
// stored JSONB rows that pre-date the #105 migration; they are discarded after
// the fan-out below.
type rateLimitSpecJSON struct {
	// Current fields
	Rules   []RateLimitRule `json:"rules,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
	// Legacy fields absorbed but discarded after fan-out.
	Strategy    RateLimitStrategy `json:"strategy,omitempty"`
	Window      jsonDuration      `json:"window,omitempty"`
	Source      string            `json:"source,omitempty"`
	Description string            `json:"description,omitempty"`
	Amount      int64             `json:"amount,omitempty"`
	Meter       string            `json:"meter,omitempty"`
}

func (s *RateLimitSpec) UnmarshalJSON(b []byte) error {
	var raw rateLimitSpecJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Enabled = raw.Enabled
	s.Rules = raw.Rules
	// Legacy: lift amount+meter into a single rule.
	if len(s.Rules) == 0 && (raw.Amount != 0 || raw.Meter != "") {
		meter := raw.Meter
		if meter == "" {
			meter = string(MeterRequests)
		}
		s.Rules = []RateLimitRule{{Meter: meter, Amount: raw.Amount}}
	}
	// Legacy fan-out: fill missing per-rule window/strategy from spec-level defaults.
	for i := range s.Rules {
		if s.Rules[i].Strategy == "" && raw.Strategy != "" {
			s.Rules[i].Strategy = raw.Strategy
		}
		if s.Rules[i].Window == 0 && raw.Window != 0 {
			s.Rules[i].Window = time.Duration(raw.Window)
		}
	}
	return nil
}

func (s *RateLimitSpec) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Rules   []RateLimitRule   `yaml:"rules,omitempty"`
		Enabled *bool             `yaml:"enabled,omitempty"`
		// Legacy fields absorbed but discarded after fan-out.
		Strategy    RateLimitStrategy `yaml:"strategy,omitempty"`
		Window      time.Duration     `yaml:"window,omitempty"`
		Source      string            `yaml:"source,omitempty"`
		Description string            `yaml:"description,omitempty"`
		Amount      int64             `yaml:"amount,omitempty"`
		Meter       string            `yaml:"meter,omitempty"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	s.Enabled = raw.Enabled
	s.Rules = raw.Rules
	// Legacy: lift amount+meter into a single rule.
	if len(s.Rules) == 0 && (raw.Amount != 0 || raw.Meter != "") {
		meter := raw.Meter
		if meter == "" {
			meter = string(MeterRequests)
		}
		s.Rules = []RateLimitRule{{Meter: meter, Amount: raw.Amount}}
	}
	// Legacy fan-out: fill missing per-rule window/strategy from spec-level defaults.
	for i := range s.Rules {
		if s.Rules[i].Strategy == "" && raw.Strategy != "" {
			s.Rules[i].Strategy = raw.Strategy
		}
		if s.Rules[i].Window == 0 && raw.Window != 0 {
			s.Rules[i].Window = raw.Window
		}
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
	StrategyTokenBucket   RateLimitStrategy = "token-bucket"   // DEFAULT
	StrategySlidingWindow RateLimitStrategy = "sliding-window"
	StrategyFixedWindow   RateLimitStrategy = "fixed-window"
	StrategyLeakyBucket   RateLimitStrategy = "leaky-bucket"
	// StrategySessionWindow: timer anchors on first request after a reset,
	// runs for `window`, then idles until the next request anchors a fresh
	// window. Distinct from fixed-window (no wall-clock alignment) and from
	// token-bucket (no refill during the window — hard count, clean reset).
	// Use case: session/quota patterns like Anthropic's 5-hour limits.
	StrategySessionWindow RateLimitStrategy = "session-window"
)

// AllStrategies enumerates every accepted RateLimitStrategy. Order is stable
// and is used by the OpenAPI enum tags on RateLimit fields — keep in sync.
var AllStrategies = []RateLimitStrategy{
	StrategyTokenBucket,
	StrategySlidingWindow,
	StrategyFixedWindow,
	StrategyLeakyBucket,
	StrategySessionWindow,
}

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

	// Token sub-meters. Keys match the canonical Tokens map keys produced by
	// the shape-specific extractors in pkg/api/{openai,anthropic}/tokens.go.
	MeterTokensInput                = "tokens.input"
	MeterTokensOutput               = "tokens.output"
	MeterTokensCacheRead            = "tokens.cache_read"
	MeterTokensCacheCreation        = "tokens.cache_creation"
	MeterTokensReasoning            = "tokens.reasoning"
	MeterTokensServerToolUseInput   = "tokens.server_tool_use_input"
	MeterTokensServerToolUseOutput  = "tokens.server_tool_use_output"
)

// AllMeters enumerates every meter name the API surface accepts. The catalog
// regex (meterRE) remains permissive for back-compat; the OpenAPI enum tag
// (see RateLimitRule.Meter) enforces this set at the HTTP boundary.
var AllMeters = []string{
	string(MeterRequests),
	string(MeterConcurrency),
	string(MeterTokens),
	MeterTokensInput,
	MeterTokensOutput,
	MeterTokensCacheRead,
	MeterTokensCacheCreation,
	MeterTokensReasoning,
	MeterTokensServerToolUseInput,
	MeterTokensServerToolUseOutput,
}

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
	// PassthroughAllowed, when true, lets this key carry an upstream
	// Authorization header that relay forwards verbatim. Gated by the global
	// Passthrough singleton: takes effect only when Passthrough.spec.enabled
	// is true. Default false; passthrough is opt-in per key.
	PassthroughAllowed bool `yaml:"passthroughAllowed,omitempty" json:"passthroughAllowed,omitempty"`
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

// AllowsModel returns true when modelName is callable through the passthrough
// flow under this config. Mode="all" admits everything; "allowlist" requires
// an exact match on Models.Allow.
func (p *Passthrough) AllowsModel(modelName string) bool {
	if p == nil {
		return false
	}
	switch p.Spec.Models.Mode {
	case PassthroughModelsModeAll:
		return true
	case PassthroughModelsModeAllowlist:
		for _, m := range p.Spec.Models.Allow {
			if m == modelName {
				return true
			}
		}
		return false
	default:
		return false
	}
}

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

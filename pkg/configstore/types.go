package configstore

import "time"

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

	ContextWindow   int `yaml:"contextWindow,omitempty"   json:"contextWindow,omitempty"`
	MaxOutputTokens int `yaml:"maxOutputTokens,omitempty" json:"maxOutputTokens,omitempty"`

	Pricing *Pricing `yaml:"pricing,omitempty" json:"pricing,omitempty"`

	KnowledgeCutoff string `yaml:"knowledgeCutoff,omitempty" json:"knowledgeCutoff,omitempty"`
	ReleaseDate     string `yaml:"releaseDate,omitempty"     json:"releaseDate,omitempty"`
	DeprecationDate string `yaml:"deprecationDate,omitempty" json:"deprecationDate,omitempty"`

	Documentation string `yaml:"documentation,omitempty" json:"documentation,omitempty"`
	License       string `yaml:"license,omitempty"       json:"license,omitempty"`

	RateLimits []RateLimitAttachment `yaml:"rateLimits,omitempty" json:"rateLimits,omitempty"`
}

type Capabilities struct {
	Chat             bool `yaml:"chat,omitempty"             json:"chat,omitempty"`
	Embeddings       bool `yaml:"embeddings,omitempty"       json:"embeddings,omitempty"`
	Tools            bool `yaml:"tools,omitempty"            json:"tools,omitempty"`
	Vision           bool `yaml:"vision,omitempty"           json:"vision,omitempty"`
	Audio            bool `yaml:"audio,omitempty"            json:"audio,omitempty"`
	Streaming        bool `yaml:"streaming,omitempty"        json:"streaming,omitempty"`
	JSONMode         bool `yaml:"jsonMode,omitempty"         json:"jsonMode,omitempty"`
	StructuredOutput bool `yaml:"structuredOutput,omitempty" json:"structuredOutput,omitempty"`
	Reasoning        bool `yaml:"reasoning,omitempty"        json:"reasoning,omitempty"`
}

type Modalities struct {
	Input  []string `yaml:"input,omitempty"  json:"input,omitempty"`
	Output []string `yaml:"output,omitempty" json:"output,omitempty"`
}

// Pricing in USD per million tokens.
type Pricing struct {
	Input       float64 `yaml:"input"                json:"input"`
	CachedInput float64 `yaml:"cachedInput,omitempty" json:"cachedInput,omitempty"`
	Output      float64 `yaml:"output"               json:"output"`
}

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

type RateLimitSpec struct {
	Strategy RateLimitStrategy `yaml:"strategy"        json:"strategy"`
	Window   time.Duration     `yaml:"window"          json:"window"`
	Amount   int64             `yaml:"amount"          json:"amount"`
	Source   RateLimitSource   `yaml:"source,omitempty" json:"source,omitempty"`
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

type RateLimitAttachment struct {
	Ref   string `yaml:"ref"   json:"ref"`
	Meter Meter  `yaml:"meter" json:"meter"`
}

type Meter string

const (
	MeterRequests    Meter = "requests"
	MeterTokens      Meter = "tokens"
	MeterConcurrency Meter = "concurrency"
)

type ResolvedRule struct {
	ParentKind Kind
	ParentName string
	Meter      Meter
	RateLimit  *RateLimit
}

package configstore

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
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

type Provider struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       Kind         `yaml:"kind"`
	Metadata   Metadata     `yaml:"metadata"`
	Spec       ProviderSpec `yaml:"spec"`
}

type ProviderSpec struct {
	Kind        ProviderKind `yaml:"kind"`
	BaseURL     string       `yaml:"baseURL"`
	Default     bool         `yaml:"default,omitempty"`
	DefaultPool string       `yaml:"defaultPool,omitempty"`
}

type Secret struct {
	APIVersion  string     `yaml:"apiVersion"`
	Kind        Kind       `yaml:"kind"`
	Metadata    Metadata   `yaml:"metadata"`
	Spec        SecretSpec `yaml:"spec"`
	Resolved    string
	KeyHash     string
	UsedLiteral bool
}

type SecretSpec struct {
	Provider  string           `yaml:"provider"`
	ValueFrom *SecretValueFrom `yaml:"valueFrom,omitempty"`
	Value     string           `yaml:"value,omitempty"`
}

type SecretValueFrom struct {
	Env string `yaml:"env"`
}

type Pool struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       Kind     `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       PoolSpec `yaml:"spec"`
}

type PoolSpec struct {
	Provider       string            `yaml:"provider"`
	Secrets        []string          `yaml:"secrets,omitempty"`
	SecretSelector map[string]string `yaml:"secretSelector,omitempty"`
}

type Model struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       ModelSpec `yaml:"spec"`
}

type ModelSpec struct {
	Provider     string `yaml:"provider"`
	UpstreamName string `yaml:"upstreamName"`

	Description string `yaml:"description,omitempty"`
	Family      string `yaml:"family,omitempty"`
	Version     string `yaml:"version,omitempty"`

	Capabilities Capabilities `yaml:"capabilities,omitempty"`
	Modalities   Modalities   `yaml:"modalities,omitempty"`

	ContextWindow   int `yaml:"contextWindow,omitempty"`
	MaxOutputTokens int `yaml:"maxOutputTokens,omitempty"`

	Pricing *Pricing `yaml:"pricing,omitempty"`

	KnowledgeCutoff string `yaml:"knowledgeCutoff,omitempty"`
	ReleaseDate     string `yaml:"releaseDate,omitempty"`
	DeprecationDate string `yaml:"deprecationDate,omitempty"`

	Documentation string `yaml:"documentation,omitempty"`
	License       string `yaml:"license,omitempty"`
}

type Capabilities struct {
	Chat             bool `yaml:"chat,omitempty"`
	Embeddings       bool `yaml:"embeddings,omitempty"`
	Tools            bool `yaml:"tools,omitempty"`
	Vision           bool `yaml:"vision,omitempty"`
	Audio            bool `yaml:"audio,omitempty"`
	Streaming        bool `yaml:"streaming,omitempty"`
	JSONMode         bool `yaml:"jsonMode,omitempty"`
	StructuredOutput bool `yaml:"structuredOutput,omitempty"`
	Reasoning        bool `yaml:"reasoning,omitempty"`
}

type Modalities struct {
	Input  []string `yaml:"input,omitempty"`
	Output []string `yaml:"output,omitempty"`
}

// Pricing in USD per million tokens.
type Pricing struct {
	Input       float64 `yaml:"input"`
	CachedInput float64 `yaml:"cachedInput,omitempty"`
	Output      float64 `yaml:"output"`
}

type Route struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       RouteSpec `yaml:"spec"`
}

type RouteSpec struct {
	Default bool     `yaml:"default,omitempty"`
	Models  []string `yaml:"models"`
}

type RateLimit struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       Kind          `yaml:"kind"`
	Metadata   Metadata      `yaml:"metadata"`
	Spec       RateLimitSpec `yaml:"spec"`
}

type RateLimitSpec struct {
	Target Target `yaml:"target"`
	RPM    int    `yaml:"rpm,omitempty"`
	TPM    int    `yaml:"tpm,omitempty"`
}

type Target struct {
	Kind Kind   `yaml:"kind"`
	Name string `yaml:"name"`
}

// Package catalog is the pure SDK view of the public relay catalog: embedded
// host/bindings/pricing data plus model-ref resolution and per-binding cost.
package catalog

import "time"

// Catalog is the embedded catalog payload written by cmd/catalog-embed.
//
// Hosts carry the routing/pricing bindings (the call-path shape). Models and
// Providers are the normalized rich-metadata sidecars — emitted once per slug,
// not duplicated onto every (model, host) binding — that the discovery graph
// (sdk/internal/graph, surfaced via the model/host/provider packages) joins to
// the bindings. They are display data; nothing on the request path reads them.
type Catalog struct {
	Version     string         `json:"version"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Hosts       []Host         `json:"hosts"`
	Models      []ModelInfo    `json:"models,omitempty"`
	Providers   []ProviderInfo `json:"providers,omitempty"`
}

// Host is one upstream serving endpoint and its model bindings. The display
// fields are operator-set metadata for catalog UIs; they do not affect routing.
type Host struct {
	Name    string    `json:"name"`
	BaseURL string    `json:"baseURL"`
	Models  []Binding `json:"models"`

	DisplayName   string `json:"displayName,omitempty"`
	Description   string `json:"description,omitempty"`
	HomepageURL   string `json:"homepageURL,omitempty"`
	DocsURL       string `json:"docsURL,omitempty"`
	ConsoleURL    string `json:"consoleURL,omitempty"`
	StatusPageURL string `json:"statusPageURL,omitempty"`
	Icon          string `json:"icon,omitempty"`
}

// ModelInfo is the rich per-model metadata, keyed by MetadataName (the snapshot
// slug that bindings reference). The graph joins it to the bindings to build a
// Model node; the model's real wire name + per-host pricing come from the
// bindings, so they are not repeated here.
type ModelInfo struct {
	MetadataName string `json:"metadataName"`
	Provider     string `json:"provider,omitempty"` // author provider slug

	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Family      string `json:"family,omitempty"`
	Version     string `json:"version,omitempty"`

	Capabilities Capabilities `json:"capabilities,omitempty"`
	Modalities   Modalities   `json:"modalities,omitempty"`

	ContextWindowInput  int `json:"contextWindowInput,omitempty"`
	ContextWindowOutput int `json:"contextWindowOutput,omitempty"`
	ContextWindowTotal  int `json:"contextWindowTotal,omitempty"`
	MaxOutputTokens     int `json:"maxOutputTokens,omitempty"`

	KnowledgeCutoff      string   `json:"knowledgeCutoff,omitempty"`
	ReleaseDate          string   `json:"releaseDate,omitempty"`
	License              string   `json:"license,omitempty"`
	Tags                 []string `json:"tags,omitempty"`
	Documentation        string   `json:"documentation,omitempty"`
	ProviderModelPageURL string   `json:"providerModelPageURL,omitempty"`
}

// ProviderInfo is the rich per-provider (model author) metadata, keyed by Name.
type ProviderInfo struct {
	Name          string `json:"name"`
	DisplayName   string `json:"displayName,omitempty"`
	Description   string `json:"description,omitempty"`
	HomepageURL   string `json:"homepageURL,omitempty"`
	DocsURL       string `json:"docsURL,omitempty"`
	StatusPageURL string `json:"statusPageURL,omitempty"`
	Icon          string `json:"icon,omitempty"`
}

// Capabilities mirrors app/model.Capabilities (same json tags) so the embed can
// carry it by a plain json round-trip without the SDK importing app/.
type Capabilities struct {
	Chat              bool `json:"chat,omitempty"`
	Embeddings        bool `json:"embeddings,omitempty"`
	Streaming         bool `json:"streaming,omitempty"`
	Tools             bool `json:"tools,omitempty"`
	ParallelTools     bool `json:"parallelTools,omitempty"`
	Vision            bool `json:"vision,omitempty"`
	Audio             bool `json:"audio,omitempty"`
	PromptCache       bool `json:"promptCache,omitempty"`
	Reasoning         bool `json:"reasoning,omitempty"`
	JSONMode          bool `json:"jsonMode,omitempty"`
	StructuredOutputs bool `json:"structuredOutputs,omitempty"`
	Batch             bool `json:"batch,omitempty"`
	ComputerUse       bool `json:"computerUse,omitempty"`
	WebSearch         bool `json:"webSearch,omitempty"`
	FileInput         bool `json:"fileInput,omitempty"`
	AudioInput        bool `json:"audioInput,omitempty"`
	AudioOutput       bool `json:"audioOutput,omitempty"`
	SystemMessages    bool `json:"systemMessages,omitempty"`
	AssistantPrefill  bool `json:"assistantPrefill,omitempty"`
}

// Modalities mirrors app/model.Modalities.
type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// Binding is one callable (snapshot, host) pair with wire metadata. Featured
// carries the catalog's `labels.featured` curation flag (the same one the
// control API exposes) so SDK consumers can shortlist without re-curating.
//
// Name is the model's real provider wire name: the id the provider answers to
// and echoes back as the ran model — the identity a consumer recognizes.
// MetadataName is the catalog key, the DNS-1123 snapshot slug (== catalog
// metadata.name), an internal addressing handle (e.g. name gpt-5.5-2026-04-23,
// MetadataName gpt-5-5-2026-04-23). Both are first-class matchable keys —
// consumers matching a response's model against the catalog must compare it to
// Name AND MetadataName (or just call Resolve, which indexes both). The json
// tags are unchanged from the on-wire catalog (`upstream`/`model`).
type Binding struct {
	Name         string   `json:"upstream"`
	MetadataName string   `json:"model"`
	Adapter      string   `json:"adapter"`
	Providers    []string `json:"providers"`
	Featured     bool     `json:"featured,omitempty"`
	Pricing      []Rate   `json:"pricing,omitempty"`

	// Aliases are the model's declared resolution-only matchers, attached
	// to the pointer-snapshot row: exact strings or single-'*' wildcard
	// patterns. Last-priority lookup keys for Resolve, never identity —
	// the relay routes them to this binding and passes the requested
	// string upstream verbatim.
	Aliases []string `json:"aliases,omitempty"`
}

// Rate is one priced meter on a binding.
type Rate struct {
	Meter       string  `json:"meter"`
	Unit        string  `json:"unit"`
	Amount      float64 `json:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty"`
}

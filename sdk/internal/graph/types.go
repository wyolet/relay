package graph

// Provider is a model author (the vendor). Models lists the slugs it authors.
type Provider struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName,omitempty"`
	Description   string   `json:"description,omitempty"`
	HomepageURL   string   `json:"homepageURL,omitempty"`
	DocsURL       string   `json:"docsURL,omitempty"`
	StatusPageURL string   `json:"statusPageURL,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	Models        []string `json:"models,omitempty"`
}

// Host is a serving endpoint. Models lists the slugs it serves.
type Host struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName,omitempty"`
	Description   string   `json:"description,omitempty"`
	BaseURL       string   `json:"baseURL"`
	HomepageURL   string   `json:"homepageURL,omitempty"`
	DocsURL       string   `json:"docsURL,omitempty"`
	ConsoleURL    string   `json:"consoleURL,omitempty"`
	StatusPageURL string   `json:"statusPageURL,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	Models        []string `json:"models,omitempty"`
}

// ModelHost is one place a model is served: the host node plus the per-host
// serving terms (wire adapter + pricing) that live on the (model, host) edge.
type ModelHost struct {
	Host    *Host  `json:"host"`
	Adapter string `json:"adapter,omitempty"`
	Pricing []Rate `json:"pricing,omitempty"`
}

// Rate is one priced meter (mirror of catalog.Rate; convertible field-for-field).
type Rate struct {
	Meter       string  `json:"meter"`
	Unit        string  `json:"unit"`
	Amount      float64 `json:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty"`
}

// Capabilities mirrors catalog.Capabilities field-for-field (convertible).
type Capabilities struct {
	UnsupportedParams []string `json:"unsupportedParams,omitempty"`
	Chat              bool     `json:"chat,omitempty"`
	Embeddings        bool     `json:"embeddings,omitempty"`
	Streaming         bool     `json:"streaming,omitempty"`
	Tools             bool     `json:"tools,omitempty"`
	ParallelTools     bool     `json:"parallelTools,omitempty"`
	Vision            bool     `json:"vision,omitempty"`
	Audio             bool     `json:"audio,omitempty"`
	PromptCache       bool     `json:"promptCache,omitempty"`
	Reasoning         bool     `json:"reasoning,omitempty"`
	JSONMode          bool     `json:"jsonMode,omitempty"`
	StructuredOutputs bool     `json:"structuredOutputs,omitempty"`
	Batch             bool     `json:"batch,omitempty"`
	ComputerUse       bool     `json:"computerUse,omitempty"`
	WebSearch         bool     `json:"webSearch,omitempty"`
	FileInput         bool     `json:"fileInput,omitempty"`
	AudioInput        bool     `json:"audioInput,omitempty"`
	AudioOutput       bool     `json:"audioOutput,omitempty"`
	SystemMessages    bool     `json:"systemMessages,omitempty"`
	AssistantPrefill  bool     `json:"assistantPrefill,omitempty"`
}

// Modalities mirrors catalog.Modalities (convertible).
type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// ContextWindow is the model's token-window split.
type ContextWindow struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
	Total  int `json:"total,omitempty"`
}

// Model is the discovery node: the model plus its author and the hosts serving
// it. Name is the real provider wire name; Slug is our catalog metadata name.
type Model struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Family      string `json:"family,omitempty"`
	Version     string `json:"version,omitempty"`

	Capabilities  Capabilities  `json:"capabilities,omitempty"`
	Modalities    Modalities    `json:"modalities,omitempty"`
	ContextWindow ContextWindow `json:"contextWindow,omitempty"`

	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	KnowledgeCutoff string   `json:"knowledgeCutoff,omitempty"`
	ReleaseDate     string   `json:"releaseDate,omitempty"`
	License         string   `json:"license,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Featured        bool     `json:"featured,omitempty"`
	Aliases         []string `json:"aliases,omitempty"`

	Author *Provider   `json:"author,omitempty"`
	Hosts  []ModelHost `json:"hosts,omitempty"`
}

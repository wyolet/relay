package clientprofile

import (
	"encoding/json"
	"net/http"
	"strings"
)

// OpenCode reaches a custom provider through the Vercel AI SDK, whose packages each append their own fixed path to the configured baseURL — "/chat/completions" for the openai-compatible package, "/messages" for the Anthropic one. One prefix therefore has to carry both wire shapes, so that either package works against the same base URL.
//
// Verified against opencode 1.17.11; the recorded requests are in testdata/opencode/.
type opencode struct{}

// OpenCode returns the profile for the OpenCode CLI.
func OpenCode() Profile { return opencode{} }

func (opencode) Name() string { return "opencode" }

// opencodeShapes are the inbound adapter names of the two AI SDK packages OpenCode can point at a custom provider. The chat-completions shape comes first because it is what OpenCode loads when a provider declares no `npm`.
var opencodeShapes = []string{"openai", "anthropic"}

func (opencode) Shape() string    { return opencodeShapes[0] }
func (opencode) Shapes() []string { return opencodeShapes }

// uaPrefixOpenCode matches every surface: the client builds its User-Agent as "opencode/<version> …" on the inference path and "opencode/<channel>/<version>/<client>" when it fetches its model catalog.
const uaPrefixOpenCode = "opencode/"

func (opencode) Match(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("User-Agent"), uaPrefixOpenCode)
}

// opencodeAttributionHeaders is the client's own request marker, sent on every turn of one session. x-session-affinity is deliberately left out: the recorded requests show it repeating the same id as a sticky-routing hint, not a second fact worth storing.
var opencodeAttributionHeaders = []string{"x-session-id"}

func (opencode) AttributionHeaders() []string { return opencodeAttributionHeaders }

// SessionKey reads the per-conversation marker — opencodeAttributionHeaders[0], named once so the two uses cannot drift apart.
func (opencode) SessionKey(h http.Header) string { return h.Get(opencodeAttributionHeaders[0]) }

// ModelListPath is where OpenCode reads a model catalog: the models.dev document, which it fetches as "{source}/api.json" and also accepts as a file through OPENCODE_MODELS_PATH. It is not a list-models endpoint in the vendor sense — OpenCode asks a provider for no such thing.
func (opencode) ModelListPath() string { return "/api.json" }

// The models.dev document OpenCode reads. Pinned against the effect-schema in packages/core/src/models-dev.ts on anomalyco/opencode@dev (https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/core/src/models-dev.ts) and against a live provider entry from https://models.dev/api.json. OpenCode casts the parsed JSON instead of decoding it through that schema, so an unknown key is inert and a missing one is filled from its own defaults — the required set below is what keeps a model usable, not what avoids a parse error.
type opencodeProvider struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Env  []string `json:"env"`
	NPM  string   `json:"npm"`
	// API is the provider's base URL. OpenCode uses it whenever the user's own config sets no baseURL, which is what lets a provider block be omitted entirely.
	API    string                   `json:"api,omitempty"`
	Models map[string]opencodeModel `json:"models"`
}

type opencodeModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ReleaseDate is always emitted, empty when the catalog declares none: the schema types it as a plain string and OpenCode's own fallback for a missing one is "".
	ReleaseDate string              `json:"release_date"`
	Attachment  bool                `json:"attachment"`
	Reasoning   bool                `json:"reasoning"`
	Temperature bool                `json:"temperature"`
	ToolCall    bool                `json:"tool_call"`
	Cost        *opencodeCost       `json:"cost,omitempty"`
	Limit       opencodeLimit       `json:"limit"`
	Modalities  *opencodeModalities `json:"modalities,omitempty"`
}

// Costs are USD per million tokens, the models.dev convention.
type opencodeCost struct {
	Input      float64            `json:"input"`
	Output     float64            `json:"output"`
	CacheRead  *float64           `json:"cache_read,omitempty"`
	CacheWrite *float64           `json:"cache_write,omitempty"`
	Tiers      []opencodeCostTier `json:"tiers,omitempty"`
}

type opencodeCostTier struct {
	Input      float64          `json:"input"`
	Output     float64          `json:"output"`
	CacheRead  *float64         `json:"cache_read,omitempty"`
	CacheWrite *float64         `json:"cache_write,omitempty"`
	Tier       opencodeTierSize `json:"tier"`
}

type opencodeTierSize struct {
	Type string `json:"type"`
	Size int    `json:"size"`
}

type opencodeLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type opencodeModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

const (
	// opencodeProviderID doubles as the picker prefix: a model is addressed as "relay/<id>".
	opencodeProviderID   = "relay"
	opencodeProviderName = "Wyolet Relay"
	// opencodeKeyEnv is the variable OpenCode reads the relay key from. A provider whose env var is set loads with no config block at all.
	opencodeKeyEnv = "RELAY_API_KEY"
	// opencodeNPM is the AI SDK package the document asks OpenCode to load. The chat-completions one is the default for a gateway; the Anthropic package is a per-user choice and stays in the docs rather than here.
	opencodeNPM = "@ai-sdk/openai-compatible"
	// opencodeAPIPath is where opencodeNPM's requests land, the AI SDK appending "/chat/completions" to it.
	opencodeAPIPath = "/opencode/v1"
	// opencodeTierType is the only tier kind the schema defines.
	opencodeTierType = "context"
)

// opencodeModalityKinds is the closed set the document's modality lists allow. A catalog value outside it is left out: OpenCode maps each entry onto a fixed capability flag and has nowhere to put an unknown one.
var opencodeModalityKinds = map[string]bool{"text": true, "audio": true, "image": true, "video": true, "pdf": true}

// ModelsWithContext renders the models.dev document, one provider holding every model the caller's key can route to. A model the catalog declares no context window for is left out entirely: limit.context is required, OpenCode reads a missing one as 0, and a 0 silently disables both its context-remaining display and its compaction trigger — a listed-but-broken model is worse than an absent one.
func (p opencode) ModelsWithContext(lc ListContext, entries []ModelEntry) ([]byte, string, error) {
	models := make(map[string]opencodeModel, len(entries))
	for _, e := range entries {
		if e.ContextWindow <= 0 {
			continue
		}
		models[e.ID] = opencodeModel{
			ID:          e.ID,
			Name:        entryLabel(e),
			ReleaseDate: e.ReleasedAt,
			Attachment:  opencodeAttachment(e),
			Reasoning:   e.Reasoning,
			Temperature: e.Temperature,
			ToolCall:    e.ToolCall,
			Cost:        opencodeCostOf(e.Hosts),
			Limit:       opencodeLimitOf(e),
			Modalities:  opencodeModalitiesOf(e.Modalities),
		}
	}
	body, err := json.Marshal(map[string]opencodeProvider{opencodeProviderID: {
		ID:     opencodeProviderID,
		Name:   opencodeProviderName,
		Env:    []string{opencodeKeyEnv},
		NPM:    opencodeNPM,
		API:    opencodeAPI(lc.PublicURL),
		Models: models,
	}})
	if err != nil {
		return nil, "", err
	}
	return body, "application/json", nil
}

// opencodeAPI is the base URL the document hands back to the client. Empty when the deployment publishes no public URL, which leaves OpenCode on the baseURL from the user's own config.
func opencodeAPI(publicURL string) string {
	if publicURL == "" {
		return ""
	}
	return publicURL + opencodeAPIPath
}

// opencodeLimitOf sizes a session. A model with no declared output cap falls back to its context window: OpenCode reads a missing output limit as 0 and then clamps every request to it, and it caps the value at 32k of its own accord anyway.
func opencodeLimitOf(e ModelEntry) opencodeLimit {
	out := e.MaxOutputTokens
	if out <= 0 {
		out = e.ContextWindow
	}
	return opencodeLimit{Context: e.ContextWindow, Output: out}
}

// opencodeAttachment is OpenCode's "accepts file parts" bit: the catalog says so either through the vision capability or by listing an input modality beyond text.
func opencodeAttachment(e ModelEntry) bool {
	if e.Vision {
		return true
	}
	for _, m := range e.Modalities.Input {
		if m != "text" {
			return true
		}
	}
	return false
}

func opencodeModalitiesOf(m Modalities) *opencodeModalities {
	in, out := opencodeModalityList(m.Input), opencodeModalityList(m.Output)
	if len(in) == 0 && len(out) == 0 {
		return nil
	}
	return &opencodeModalities{Input: in, Output: out}
}

func opencodeModalityList(kinds []string) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if opencodeModalityKinds[k] {
			out = append(out, k)
		}
	}
	return out
}

// opencodeCostOf prices the model off the first host that carries a rate sheet. A model served by several hosts can have several prices and the document has room for one; taking the first keeps the figure a real rate from a real route rather than an average of several. nil when no host is priced — OpenCode then shows the model with no cost rather than a fabricated zero.
func opencodeCostOf(hosts []ModelHost) *opencodeCost {
	for _, h := range hosts {
		if !h.Priced {
			continue
		}
		cost := &opencodeCost{
			Input:      h.InputUSDPerMtok,
			Output:     h.OutputUSDPerMtok,
			CacheRead:  h.CacheReadUSDPerMtok,
			CacheWrite: h.CacheWriteUSDPerMtok,
		}
		for _, t := range h.Tiers {
			cost.Tiers = append(cost.Tiers, opencodeCostTier{
				Input:      t.InputUSDPerMtok,
				Output:     t.OutputUSDPerMtok,
				CacheRead:  t.CacheReadUSDPerMtok,
				CacheWrite: t.CacheWriteUSDPerMtok,
				Tier:       opencodeTierSize{Type: opencodeTierType, Size: t.AboveTokens},
			})
		}
		return cost
	}
	return nil
}

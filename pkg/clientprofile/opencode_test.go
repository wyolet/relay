package clientprofile

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestOpenCode_Identity(t *testing.T) {
	p := OpenCode()
	if p.Name() != "opencode" {
		t.Errorf("Name = %q", p.Name())
	}
	// Shape names the primary of the two, and must be the first shape.
	if p.Shape() != "openai" {
		t.Errorf("Shape = %q", p.Shape())
	}
	ms, ok := p.(MultiShape)
	if !ok {
		t.Fatal("OpenCode must implement MultiShape")
	}
	if got := ms.Shapes(); len(got) != 2 || got[0] != "openai" || got[1] != "anthropic" {
		t.Errorf("Shapes = %v", got)
	}
	if got := ms.Shapes()[0]; got != p.Shape() {
		t.Errorf("Shape %q must be Shapes()[0] %q", p.Shape(), got)
	}
	if _, ok := p.(Attributor); !ok {
		t.Error("OpenCode must implement Attributor")
	}
	if _, ok := p.(SessionKeyer); !ok {
		t.Error("OpenCode must implement SessionKeyer")
	}
	if _, ok := p.(ListerWithContext); !ok {
		t.Error("OpenCode must implement ListerWithContext")
	}
	if r, ok := p.(ModelListRoute); !ok || r.ModelListPath() != "/api.json" {
		t.Error("OpenCode must serve its catalog at /api.json")
	}
	// The client probes no endpoint, needs no id reshaping, and asks no
	// gateway to count tokens.
	if _, ok := p.(Router); ok {
		t.Error("OpenCode must not implement Router")
	}
	if _, ok := p.(ModelNamer); ok {
		t.Error("OpenCode must not implement ModelNamer")
	}
	if _, ok := p.(TokenCountRoute); ok {
		t.Error("OpenCode must not implement TokenCountRoute")
	}
}

func TestSpeaks(t *testing.T) {
	if !Speaks(OpenCode(), "anthropic") || !Speaks(OpenCode(), "openai") {
		t.Error("a multi-shape profile speaks every shape it names")
	}
	if Speaks(OpenCode(), "gemini") {
		t.Error("a multi-shape profile speaks nothing else")
	}
	if !Speaks(Codex(), "openai_responses") || Speaks(Codex(), "openai") {
		t.Error("a single-shape profile speaks its shape and nothing else")
	}
	if Speaks(Default, "openai") || Speaks(OpenCode(), "") {
		t.Error("neither the default profile nor an empty shape matches")
	}
}

func TestOpenCode_Match(t *testing.T) {
	cases := map[string]bool{
		"opencode/1.17.11 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14": true,
		"opencode/dev/1.18.31/cli":               true,
		"claude-cli/2.1.278 (external, sdk-cli)": false,
		"codex_cli_rs/0.153.4":                   false,
		"Bun/1.4.3":                              false,
		"":                                       false,
	}
	for ua, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if got := OpenCode().Match(r); got != want {
			t.Errorf("Match(User-Agent=%q) = %v, want %v", ua, got, want)
		}
	}
}

func TestOpenCode_SessionKey(t *testing.T) {
	keyer := OpenCode().(SessionKeyer)
	h := http.Header{}
	if got := keyer.SessionKey(h); got != "" {
		t.Errorf("SessionKey without the header = %q, want empty", got)
	}
	h.Set("X-Session-Id", "ses_f3db8ed8cffeAIJhUlbxDbOx4l")
	if got := keyer.SessionKey(h); got != "ses_f3db8ed8cffeAIJhUlbxDbOx4l" {
		t.Errorf("SessionKey = %q", got)
	}
}

func cacheRate(v float64) *float64 { return &v }

// projectionEntries is the listing the document tests render: one fully
// described model with tiered pricing, one the catalog knows little about,
// and one with no context window.
func projectionEntries() []ModelEntry {
	return []ModelEntry{
		{
			ID:              "big-model-2026",
			Model:           "big-model",
			DisplayName:     "Big Model",
			Pointer:         true,
			ContextWindow:   1000000,
			MaxOutputTokens: 128000,
			Reasoning:       true,
			ToolCall:        true,
			Temperature:     true,
			ReleasedAt:      "2026-02-17",
			Modalities:      Modalities{Input: []string{"text", "image", "pdf", "hologram"}, Output: []string{"text"}},
			Hosts: []ModelHost{
				{Name: "unpriced-host"},
				{
					Name: "priced-host", Priced: true,
					InputUSDPerMtok: 3, OutputUSDPerMtok: 15,
					CacheReadUSDPerMtok:  cacheRate(0.3),
					CacheWriteUSDPerMtok: cacheRate(3.75),
					Tiers: []PriceTier{{
						AboveTokens: 200000, InputUSDPerMtok: 6, OutputUSDPerMtok: 22.5,
						CacheReadUSDPerMtok: cacheRate(0.6),
					}},
				},
			},
		},
		{ID: "gemma4-e4b-0731", Model: "gemma4-e4b", DisplayName: "Gemma 4 E4B", ContextWindow: 128000},
		{ID: "windowless", Model: "windowless", DisplayName: "Windowless", MaxOutputTokens: 4096},
	}
}

func renderOpenCode(t *testing.T, lc ListContext, entries []ModelEntry) map[string]opencodeProvider {
	t.Helper()
	body, contentType, err := OpenCode().(ListerWithContext).ModelsWithContext(lc, entries)
	if err != nil {
		t.Fatalf("ModelsWithContext: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("contentType = %q", contentType)
	}
	var doc map[string]opencodeProvider
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal document: %v\n%s", err, body)
	}
	return doc
}

func TestOpenCode_ProviderEntry(t *testing.T) {
	doc := renderOpenCode(t, ListContext{PublicURL: "https://relay.example.com"}, projectionEntries())

	if len(doc) != 1 {
		t.Fatalf("document holds %d providers, want one", len(doc))
	}
	p, ok := doc["relay"]
	if !ok {
		t.Fatalf("document keys = %v, want relay", doc)
	}
	if p.ID != "relay" || p.Name != "Wyolet Relay" {
		t.Errorf("provider identity = %q / %q", p.ID, p.Name)
	}
	// The env list is what makes the provider load with no config block at
	// all, and npm is what makes OpenCode load the chat-completions SDK.
	if len(p.Env) != 1 || p.Env[0] != "RELAY_API_KEY" {
		t.Errorf("env = %v", p.Env)
	}
	if p.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("npm = %q", p.NPM)
	}
	if p.API != "https://relay.example.com/opencode/v1" {
		t.Errorf("api = %q", p.API)
	}
}

// Without a public URL there is nothing honest to print, and OpenCode then
// keeps the baseURL from the user's own config.
func TestOpenCode_ProviderAPIOmittedWithoutPublicURL(t *testing.T) {
	doc := renderOpenCode(t, ListContext{}, projectionEntries())
	if api := doc["relay"].API; api != "" {
		t.Errorf("api = %q, want empty", api)
	}
}

func TestOpenCode_ModelEntry(t *testing.T) {
	doc := renderOpenCode(t, ListContext{PublicURL: "https://relay.example.com"}, projectionEntries())
	models := doc["relay"].Models

	// The windowless model is not listed: OpenCode reads a missing
	// limit.context as zero and quietly loses compaction with it.
	if len(models) != 2 {
		t.Fatalf("models = %v, want the two with a context window", models)
	}
	if _, listed := models["windowless"]; listed {
		t.Error("a model with no context window must not be listed")
	}

	m := models["big-model-2026"]
	if m.ID != "big-model-2026" || m.Name != "Big Model" {
		t.Errorf("identity = %q / %q", m.ID, m.Name)
	}
	if m.ReleaseDate != "2026-02-17" {
		t.Errorf("release_date = %q", m.ReleaseDate)
	}
	if !m.Reasoning || !m.ToolCall || !m.Temperature {
		t.Errorf("capabilities = %+v", m)
	}
	if !m.Attachment {
		t.Error("a model reading images accepts attachments")
	}
	if m.Limit.Context != 1000000 || m.Limit.Output != 128000 {
		t.Errorf("limit = %+v", m.Limit)
	}
	// A modality the document's own vocabulary has no slot for is left out.
	if got := strings.Join(m.Modalities.Input, ","); got != "text,image,pdf" {
		t.Errorf("modalities.input = %q", got)
	}

	if m.Cost == nil {
		t.Fatal("a priced host must price the model")
	}
	if m.Cost.Input != 3 || m.Cost.Output != 15 {
		t.Errorf("cost = %+v", m.Cost)
	}
	if m.Cost.CacheRead == nil || *m.Cost.CacheRead != 0.3 || m.Cost.CacheWrite == nil || *m.Cost.CacheWrite != 3.75 {
		t.Errorf("cache rates = %+v", m.Cost)
	}
	if len(m.Cost.Tiers) != 1 {
		t.Fatalf("tiers = %+v", m.Cost.Tiers)
	}
	tier := m.Cost.Tiers[0]
	if tier.Tier.Type != "context" || tier.Tier.Size != 200000 || tier.Input != 6 || tier.Output != 22.5 {
		t.Errorf("tier = %+v", tier)
	}
	if tier.CacheWrite != nil {
		t.Error("an unpriced meter must stay absent from a tier, not read as free")
	}
}

// A model the catalog says little about still lists: the missing output cap
// falls back to the window rather than a zero OpenCode would clamp to.
func TestOpenCode_SparseModelEntry(t *testing.T) {
	doc := renderOpenCode(t, ListContext{}, projectionEntries())
	m := doc["relay"].Models["gemma4-e4b-0731"]

	if m.Name != "Gemma 4 E4B (0731)" {
		t.Errorf("name = %q, want the snapshot-suffixed label", m.Name)
	}
	if m.Limit.Context != 128000 || m.Limit.Output != 128000 {
		t.Errorf("limit = %+v", m.Limit)
	}
	if m.ReleaseDate != "" {
		t.Errorf("release_date = %q, want empty rather than invented", m.ReleaseDate)
	}
	if m.Cost != nil {
		t.Errorf("cost = %+v, want absent rather than zero", m.Cost)
	}
	if m.Modalities != nil {
		t.Errorf("modalities = %+v, want absent", m.Modalities)
	}
	if m.Attachment {
		t.Error("a model declaring no vision and no media input takes no attachments")
	}
}

// An empty listing is still a well-formed provider — OpenCode reads it as a
// provider with no models rather than failing to parse.
func TestOpenCode_EmptyListing(t *testing.T) {
	body, _, err := OpenCode().(ListerWithContext).ModelsWithContext(ListContext{}, nil)
	if err != nil {
		t.Fatalf("ModelsWithContext: %v", err)
	}
	const want = `{"relay":{"id":"relay","name":"Wyolet Relay","env":["RELAY_API_KEY"],"npm":"@ai-sdk/openai-compatible","models":{}}}`
	if string(body) != want {
		t.Errorf("empty catalog = %s", body)
	}
}

// TestOpenCode_AgainstRecordedRequests pins the profile to what the real
// client sends; testdata/opencode/README says how to refresh it.
func TestOpenCode_AgainstRecordedRequests(t *testing.T) {
	raw, err := os.ReadFile("testdata/opencode/requests.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fixture := string(raw)

	// One base URL, both wire shapes — the reason the profile is multi-shape.
	for _, line := range []string{
		"=== POST /opencode/v1/chat/completions",
		"=== POST /opencode/v1/messages",
	} {
		if !strings.Contains(fixture, line) {
			t.Errorf("fixture missing %q", line)
		}
	}

	const ua = "opencode/1.17.11 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	if !strings.Contains(fixture, "User-Agent: "+ua) {
		t.Errorf("fixture missing User-Agent %q", ua)
	}
	r := httptest.NewRequest(http.MethodPost, "/opencode/v1/chat/completions", nil)
	r.Header.Set("User-Agent", ua)
	if !OpenCode().Match(r) {
		t.Errorf("Match must accept the recorded User-Agent %q", ua)
	}

	lower := strings.ToLower(fixture)
	for _, n := range opencodeAttributionHeaders {
		if n != strings.ToLower(n) {
			t.Errorf("header name %q must be lowercase — it doubles as the metadata key", n)
		}
		if !strings.Contains(lower, "\n"+n+":") {
			t.Errorf("fixture missing header %q", n)
		}
	}
}

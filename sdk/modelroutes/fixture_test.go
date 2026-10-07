package modelroutes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

func rate(meter string, amount float64) catalog.Rate {
	return catalog.Rate{Meter: meter, Unit: "per_million", Amount: amount}
}

// fixtureCatalog serves claude at its author's host and through an aggregator, gpt at its author's host, and glm only at hosts that are not its author's, so a hostless route has to fall back to the featured one.
func fixtureCatalog(version string) *catalog.Catalog {
	return &catalog.Catalog{
		Version: version,
		Hosts: []catalog.Host{
			{Name: "openrouter", BaseURL: "https://openrouter.example/api/v1", Models: []catalog.Binding{
				{Name: "anthropic/claude-opus-5-5", MetadataName: "claude-opus-5-5", Adapter: "openai", Providers: []string{"anthropic"}, Featured: true,
					Pricing: []catalog.Rate{rate("tokens.input", 6), rate("tokens.output", 30)}},
				{Name: "z-ai/glm-5-3", MetadataName: "glm-5-3", Adapter: "openai", Providers: []string{"zhipu"}, Featured: true,
					Pricing: []catalog.Rate{rate("tokens.input", 0.6), rate("tokens.output", 2.2)}},
			}},
			{Name: "anthropic", BaseURL: "https://anthropic.example", Models: []catalog.Binding{
				{Name: "claude-opus-5-5", MetadataName: "claude-opus-5-5", Adapter: "anthropic", Providers: []string{"anthropic"},
					Pricing: []catalog.Rate{rate("tokens.input", 5), rate("tokens.output", 25)}},
			}},
			{Name: "openai", BaseURL: "https://openai.example/v1", Models: []catalog.Binding{
				{Name: "gpt-5.5", MetadataName: "gpt-5-5", Adapter: "openai_responses", Providers: []string{"openai"},
					Pricing: []catalog.Rate{rate("tokens.input", 1.25), rate("tokens.output", 10)}},
			}},
			{Name: "ollama-cloud", BaseURL: "https://ollama-cloud.example", Models: []catalog.Binding{
				{Name: "glm-5.3", MetadataName: "glm-5-3", Adapter: "openai", Providers: []string{"zhipu"}},
			}},
		},
		Models: []catalog.ModelInfo{
			{MetadataName: "claude-opus-5-5", Provider: "anthropic", DisplayName: "Claude Opus 5.5",
				Capabilities: catalog.Capabilities{Chat: true, Tools: true, Reasoning: true,
					ReasoningEfforts: []string{"low", "medium", "high"}, DefaultReasoningEffort: "high"},
				ContextWindowInput: 200000, ContextWindowOutput: 64000},
			{MetadataName: "gpt-5-5", Provider: "openai", DisplayName: "GPT-5.5"},
			{MetadataName: "glm-5-3", Provider: "zhipu", DisplayName: "GLM-5.3"},
		},
	}
}

func fixtureIndex(t *testing.T, c *catalog.Catalog) *catalog.IndexedCatalog {
	t.Helper()
	ic, err := catalog.Index(c)
	if err != nil {
		t.Fatal(err)
	}
	return ic
}

const sampleFile = `# Models the picker shows.
catalog = "v0.1.10" # pinned for CI
default = "claude-opus-5-5"

[routes]
claude-home = { host = "anthropic", auth = "subscription" }
relay       = { url = "https://relay.example" }
claude-work = { host = "anthropic", auth = "subscription" }
codex       = { host = "openai", auth = "subscription", adapter = "codex", url = "https://codex.example" }
myollama    = { url = "https://ollama.example", adapter = "openai", passthrough = true }

[models]
"claude-opus-5-5" = "claude-home"
"opus-work"       = { model = "claude-opus-5-5", route = "claude-work" }
"gpt-5-5"         = "codex"
"glm-5-3"         = "relay"
"qwen3.8:27b"     = "myollama" # self-hosted tag

# trailing note
`

func writeRoutes(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

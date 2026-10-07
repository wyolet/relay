package modeldir

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

func rate(meter string, amount float64) catalog.Rate {
	return catalog.Rate{Meter: meter, Unit: "per_million", Amount: amount}
}

// fixtureCatalog serves four models across four hosts, shaped to exercise each binding-choice rule: claude and gpt at their author's host, llama only featured elsewhere, mixtral with no featured host.
func fixtureCatalog(version string) *catalog.Catalog {
	claudeAliases := []string{"claude-opus-5-5[1m]"}
	return &catalog.Catalog{
		Version: version,
		Hosts: []catalog.Host{
			{Name: "openrouter", BaseURL: "https://openrouter.example/api/v1", Models: []catalog.Binding{
				{Name: "anthropic/claude-opus-5-5", MetadataName: "claude-opus-5-5", Adapter: "openai", Providers: []string{"anthropic"}, Featured: true, Aliases: claudeAliases,
					Pricing: []catalog.Rate{rate("tokens.input", 6), rate("tokens.output", 30)}},
				{Name: "meta-llama/llama-4-maverick", MetadataName: "llama-4-maverick", Adapter: "openai", Providers: []string{"meta"},
					Pricing: []catalog.Rate{rate("tokens.input", 0.3), rate("tokens.output", 0.9)}},
				{Name: "mistralai/mixtral-8x22b", MetadataName: "mixtral-8x22b", Adapter: "openai", Providers: []string{"mistral"},
					Pricing: []catalog.Rate{rate("tokens.input", 2), rate("tokens.output", 6)}},
			}},
			{Name: "anthropic", BaseURL: "https://anthropic.example", Models: []catalog.Binding{
				{Name: "claude-opus-5-5", MetadataName: "claude-opus-5-5", Adapter: "anthropic", Providers: []string{"anthropic"}, Aliases: claudeAliases,
					Pricing: []catalog.Rate{
						rate("tokens.input", 5),
						{Meter: "tokens.input", Unit: "per_million", Amount: 10, AboveTokens: 200000},
						rate("tokens.output", 25),
						rate("tokens.cache_read", 0.5),
					}},
			}},
			{Name: "groq", BaseURL: "https://groq.example/openai/v1", Models: []catalog.Binding{
				{Name: "llama-4-maverick-17b", MetadataName: "llama-4-maverick", Adapter: "openai", Providers: []string{"meta"}, Featured: true,
					Pricing: []catalog.Rate{rate("tokens.input", 0.2), rate("tokens.output", 0.6)}},
				{Name: "mixtral-8x22b-32768", MetadataName: "mixtral-8x22b", Adapter: "openai", Providers: []string{"mistral"},
					Pricing: []catalog.Rate{rate("tokens.input", 1.2), rate("tokens.output", 1.2)}},
			}},
			{Name: "openai", BaseURL: "https://openai.example/v1", Models: []catalog.Binding{
				{Name: "gpt-5.5-2026-04-23", MetadataName: "gpt-5-5-2026-04-23", Adapter: "openai_responses", Providers: []string{"openai"},
					Pricing: []catalog.Rate{rate("tokens.input", 1.25), rate("tokens.output", 10)}},
			}},
		},
		Models: []catalog.ModelInfo{
			{MetadataName: "claude-opus-5-5", Provider: "anthropic", DisplayName: "Claude Opus 5.5",
				Capabilities:       catalog.Capabilities{Chat: true, Tools: true, Reasoning: true, PromptCache: true, Vision: true},
				Modalities:         catalog.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
				ContextWindowInput: 1000000, ContextWindowOutput: 128000, MaxOutputTokens: 128000},
			{MetadataName: "llama-4-maverick", Provider: "meta", DisplayName: "Llama 4 Maverick", ContextWindowInput: 1000000},
			{MetadataName: "mixtral-8x22b", Provider: "mistral", DisplayName: "Mixtral 8x22B"},
			{MetadataName: "gpt-5-5-2026-04-23", Provider: "openai", DisplayName: "GPT-5.5",
				Capabilities:       catalog.Capabilities{Chat: true, Tools: true, UnsupportedParams: []string{"temperature"}},
				ContextWindowInput: 272000, ContextWindowOutput: 128000, ContextWindowTotal: 400000},
		},
	}
}

func catalogJSON(t *testing.T, c *catalog.Catalog) []byte {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fixtureSource writes c to a temp catalog file and returns a File source over it.
func fixtureSource(t *testing.T, c *catalog.Catalog) Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, catalogJSON(t, c), 0o644); err != nil {
		t.Fatal(err)
	}
	return File(path)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

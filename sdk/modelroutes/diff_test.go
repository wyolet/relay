package modelroutes

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

func TestDiff(t *testing.T) {
	body := strings.Replace(sampleFile, "[models]", `router = { host = "openrouter" }
cloud  = { host = "ollama-cloud", adapter = "openai", passthrough = true }

[models]
"claude-router" = { model = "claude-opus-5-5", route = "router" }
"qwen-next"     = "cloud"`, 1)
	f, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	to := fixtureCatalog("v0.1.11")
	host := func(name string) *catalog.Host {
		for i := range to.Hosts {
			if to.Hosts[i].Name == name {
				return &to.Hosts[i]
			}
		}
		t.Fatalf("no host %s", name)
		return nil
	}
	claude := &host("anthropic").Models[0]
	claude.Pricing = []catalog.Rate{rate("tokens.input", 6), rate("tokens.output", 25), rate("tokens.cache_read", 0.5)}
	to.Models[0].ContextWindowInput = 1000000
	to.Models[0].Capabilities.Vision = true
	to.Models[0].Capabilities.ReasoningEfforts = []string{"low", "medium", "high", "max"}
	to.Models[0].Capabilities.DefaultReasoningEffort = "medium"
	to.Models[1].Capabilities.ReasoningEfforts = []string{"low", "high"}
	gpt := &host("openai").Models[0]
	gpt.Name, gpt.Adapter = "gpt-5.5-2026", "openai"
	// openrouter drops claude and glm, so glm's default binding moves to ollama-cloud.
	host("openrouter").Models = nil
	cloud := host("ollama-cloud")
	cloud.Models = append(cloud.Models, catalog.Binding{Name: "qwen-next", MetadataName: "qwen-next", Adapter: "openai"})

	got := Diff(f, fixtureIndex(t, fixtureCatalog("v0.1.10")), fixtureIndex(t, to))
	claudeChanges := func(key string) []Change {
		return []Change{
			{key, KindPrice, "tokens.input 5 per_million → 6 per_million; tokens.cache_read none → 0.5 per_million"},
			{key, KindWindow, "input 200000 → 1000000"},
			{key, KindEffort, "reasoningEfforts [low medium high] → [low medium high max]; defaultReasoningEffort high → medium"},
			{key, KindCapabilities, "vision false → true"},
		}
	}
	var want []Change
	want = append(want, Change{"claude-router", KindRemoved, "not served on host openrouter in catalog v0.1.11"})
	want = append(want, Change{"qwen-next", KindAdded, "served on host ollama-cloud in catalog v0.1.11"})
	want = append(want, claudeChanges("claude-opus-5-5")...)
	want = append(want, claudeChanges("opus-work")...)
	want = append(want,
		Change{"gpt-5-5", KindWireName, "gpt-5.5 → gpt-5.5-2026"},
		Change{"gpt-5-5", KindAdapter, "openai_responses → openai"},
		Change{"gpt-5-5", KindEffort, "reasoningEfforts none → [low high]"},
		Change{"glm-5-3", KindHost, "openrouter → ollama-cloud"},
		Change{"glm-5-3", KindWireName, "z-ai/glm-5-3 → glm-5.3"},
		Change{"glm-5-3", KindPrice, "tokens.input 0.6 per_million → none; tokens.output 2.2 per_million → none"},
	)
	if !slices.Equal(got, want) {
		t.Fatalf("changes:\n%s\nwant:\n%s", changeLines(got), changeLines(want))
	}

	if same := Diff(f, fixtureIndex(t, fixtureCatalog("v0.1.10")), fixtureIndex(t, fixtureCatalog("v0.1.11"))); len(same) != 0 {
		t.Fatalf("identical catalogs: %+v", same)
	}
}

func changeLines(changes []Change) string {
	var b strings.Builder
	for _, c := range changes {
		b.WriteString(c.Key + " " + c.Kind + ": " + c.Detail + "\n")
	}
	return b.String()
}

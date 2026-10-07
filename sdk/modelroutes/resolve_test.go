package modelroutes

import (
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

func TestResolve(t *testing.T) {
	f, err := Parse([]byte(sampleFile))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Resolve(f, fixtureIndex(t, fixtureCatalog("v0.1.10")))
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		key, model, host, wire string
		cataloged              bool
		adapter, baseURL, path string
	}
	// gpt-5-5's route sets url, so the openai host's path is dropped.
	wants := []want{
		{"claude-opus-5-5", "claude-opus-5-5", "anthropic", "claude-opus-5-5", true, "anthropic", "https://anthropic.example", "/custom/messages"},
		{"opus-work", "claude-opus-5-5", "anthropic", "claude-opus-5-5", true, "anthropic", "https://anthropic.example", "/custom/messages"},
		{"gpt-5-5", "gpt-5-5", "openai", "gpt-5.5", true, "codex", "https://codex.example", ""},
		{"glm-5-3", "glm-5-3", "openrouter", "z-ai/glm-5-3", true, "openai", "https://relay.example", ""},
		{"qwen3.8:27b", "qwen3.8:27b", "", "qwen3.8:27b", false, "openai", "https://ollama.example", ""},
	}
	if len(entries) != len(wants) {
		t.Fatalf("entries = %+v", entries)
	}
	for i, w := range wants {
		e := entries[i]
		adapter, baseURL, path := e.Endpoint()
		got := want{e.Key, e.Model, e.Host.Name, e.WireName(), e.Cataloged, adapter, baseURL, path}
		if got != w {
			t.Errorf("entry %d = %+v, want %+v", i, got, w)
		}
	}
	if entries[1].Route.Name != "claude-work" || entries[1].Info.DisplayName != "Claude Opus 5.5" {
		t.Fatalf("opus-work = %+v", entries[1])
	}
	if entries[3].Info.DisplayName != "GLM-5.3" {
		t.Fatalf("hostless route lost model info: %+v", entries[3].Info)
	}
}

func TestResolvePassthroughOnHost(t *testing.T) {
	f, err := Parse([]byte(`catalog = "v0.1.10"
[routes]
cloud = { host = "ollama-cloud", adapter = "openai", passthrough = true }
[models]
"glm-5-3" = "cloud"
"qwen-next" = "cloud"
`))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Resolve(f, fixtureIndex(t, fixtureCatalog("v0.1.10")))
	if err != nil {
		t.Fatal(err)
	}
	if hit := entries[0]; !hit.Cataloged || hit.WireName() != "glm-5.3" {
		t.Fatalf("catalog hit = %+v", hit)
	}
	miss := entries[1]
	if adapter, baseURL, path := miss.Endpoint(); miss.Cataloged || miss.Binding.Name != "" || adapter != "openai" || baseURL != "https://ollama-cloud.example" || path != "" {
		t.Fatalf("catalog miss = %+v (endpoint %s %s %q)", miss, adapter, baseURL, path)
	}
	if miss.WireName() != "qwen-next" {
		t.Fatalf("catalog miss wire name = %q, want the ref verbatim", miss.WireName())
	}
}

// An ambiguous ref is a catalog problem, not an absent model: it must surface as is, and a passthrough route must not swallow it.
func TestResolveReportsAmbiguousRef(t *testing.T) {
	c := fixtureCatalog("v0.1.10")
	c.Hosts[0].Models = append(c.Hosts[0].Models, catalog.Binding{Name: "shared", MetadataName: "model-a", Adapter: "openai"})
	c.Hosts[1].Models = append(c.Hosts[1].Models, catalog.Binding{Name: "shared", MetadataName: "model-b", Adapter: "anthropic"})
	ic := fixtureIndex(t, c)
	for _, route := range []string{"relay", "myollama"} {
		f, err := Parse([]byte(strings.Replace(sampleFile, `"glm-5-3"         = "relay"`, `"shared" = "`+route+`"`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Resolve(f, ic)
		if err == nil || !strings.Contains(err.Error(), "multiple models") {
			t.Fatalf("route %s: err = %v, want the ambiguity reported", route, err)
		}
	}
}

func TestResolveReportsEveryProblem(t *testing.T) {
	f, err := Parse([]byte(`catalog = ""
default = "nope"

[routes]
empty   = { auth = "key" }
ghost   = { host = "no-such-host" }
badauth = { url = "https://x.example", auth = "oauth" }
loose   = { url = "https://x.example", passthrough = true }
claude  = { host = "anthropic" }

[models]
"gpt-5-5"       = "claude"
"glm-5-3"       = "missing"
"on-ghost"      = "ghost"
"claude-opus-5-5" = "claude"
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(f, fixtureIndex(t, fixtureCatalog("v0.1.10")))
	if err == nil {
		t.Fatal("want error")
	}
	wants := []string{
		`line 1: catalog is empty`,
		`line 5: route "empty": needs a host or a url`,
		`line 6: route "ghost": unknown catalog host "no-such-host"`,
		`line 7: route "badauth": auth "oauth"`,
		`line 8: route "loose": passthrough needs an adapter`,
		`line 12: model "gpt-5-5": not served on host "anthropic"`,
		`line 13: model "glm-5-3": unknown route "missing"`,
		`line 2: default "nope" is not a [models] key`,
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) != len(wants) {
		t.Fatalf("want %d problems, got:\n%v", len(wants), err)
	}
}

func TestResolveDuplicateKeyInBuiltFile(t *testing.T) {
	f := &File{
		Catalog: "v0.1.10",
		Routes:  []Route{{Name: "claude", Host: "anthropic"}},
		Models:  []ModelRoute{{Key: "claude-opus-5-5", Route: "claude"}, {Key: "claude-opus-5-5", Route: "claude"}},
	}
	if _, err := Resolve(f, fixtureIndex(t, fixtureCatalog("v0.1.10"))); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("err = %v, want duplicate key", err)
	}
}

package modelroutes

import (
	"slices"
	"strings"
	"testing"
)

func TestParseKeepsFileOrder(t *testing.T) {
	f, err := Read(writeRoutes(t, sampleFile))
	if err != nil {
		t.Fatal(err)
	}
	if f.Catalog != "v0.1.10" || f.Default != "claude-opus-5-5" {
		t.Fatalf("catalog = %q, default = %q", f.Catalog, f.Default)
	}
	wantRoutes := []Route{
		{Name: "claude-home", Host: "anthropic", Auth: "subscription"},
		{Name: "relay", URL: "https://relay.example"},
		{Name: "claude-work", Host: "anthropic", Auth: "subscription"},
		{Name: "codex", Host: "openai", Auth: "subscription", Adapter: "codex", URL: "https://codex.example"},
		{Name: "myollama", URL: "https://ollama.example", Adapter: "openai", Passthrough: true},
	}
	if !slices.Equal(f.Routes, wantRoutes) {
		t.Fatalf("routes = %+v", f.Routes)
	}
	wantModels := []ModelRoute{
		{Key: "claude-opus-5-5", Model: "claude-opus-5-5", Route: "claude-home"},
		{Key: "opus-work", Model: "claude-opus-5-5", Route: "claude-work"},
		{Key: "gpt-5-5", Model: "gpt-5-5", Route: "codex"},
		{Key: "glm-5-3", Model: "glm-5-3", Route: "relay"},
		{Key: "qwen3.8:27b", Model: "qwen3.8:27b", Route: "myollama"},
	}
	if !slices.Equal(f.Models, wantModels) {
		t.Fatalf("models = %+v", f.Models)
	}
}

func TestParseTableForms(t *testing.T) {
	f, err := Parse([]byte(`catalog = "v1"
[routes.b]
url = "https://b.example"
[routes]
a.url = "https://a.example"
[models]
zeta = "b"
alpha = "a"
beta.model = "m"
beta.route = "b"
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Routes[0].Name != "b" || f.Routes[1].Name != "a" {
		t.Fatalf("route order lost: %+v", f.Routes)
	}
	want := []ModelRoute{{"zeta", "zeta", "b"}, {"alpha", "alpha", "a"}, {"beta", "m", "b"}}
	if !slices.Equal(f.Models, want) {
		t.Fatalf("models = %+v", f.Models)
	}
}

func TestParseAccountAndDisplayName(t *testing.T) {
	f, err := Parse([]byte(`catalog = "v1"
[routes]
inline = { host = "anthropic", account = "dev@example.com", displayName = "Claude (dev)" }
[routes.table]
url = "https://t.example"
account = "ops@example.com"
displayName = "Ops box"
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Route{
		{Name: "inline", Host: "anthropic", Account: "dev@example.com", DisplayName: "Claude (dev)"},
		{Name: "table", URL: "https://t.example", Account: "ops@example.com", DisplayName: "Ops box"},
	}
	if !slices.Equal(f.Routes, want) {
		t.Fatalf("routes = %+v", f.Routes)
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`catalog = "v1"
colour = "red"
[routes]
r = { url = "https://r.example", proxy = true }
[models]
a = "r"
b = 3
c = { model = "x" }
d = { model = "x", route = "r", tier = 1 }
`))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{
		`line 2: unknown key colour`,
		`line 4: unknown key routes.r.proxy`,
		`line 7: models."b": want a route name`,
		`line 8: models."c": needs both model and route`,
		`line 9: unknown key models.d.tier`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestParseSyntaxError(t *testing.T) {
	if _, err := Parse([]byte("[models]\na = \"x\"\na = \"y\"\n")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("duplicate key: err = %v, want a line-numbered error", err)
	}
}

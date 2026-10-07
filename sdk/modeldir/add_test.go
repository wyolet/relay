package modeldir

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestAddWritesFile(t *testing.T) {
	dir := t.TempDir()
	src := fixtureSource(t, fixtureCatalog("v0.1.10"))

	m, err := Add(context.Background(), dir, "claude-opus-5-5[1m]", src, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := `name: claude-opus-5-5
wireName: claude-opus-5-5
provider: anthropic
adapter: anthropic
displayName: Claude Opus 5.5
aliases: ['claude-opus-5-5[1m]']
contextWindow: {input: 1000000, output: 128000}
maxOutputTokens: 128000
capabilities: {chat: true, tools: true, vision: true, promptCache: true, reasoning: true}
modalities: {input: [text, image], output: [text]}
pricing:
  - {meter: tokens.input, unit: per_million, amount: 5}
  - {meter: tokens.input, unit: per_million, amount: 10, aboveTokens: 200000}
  - {meter: tokens.output, unit: per_million, amount: 25}
  - {meter: tokens.cache_read, unit: per_million, amount: 0.5}
pricedBy: anthropic
source: {catalog: v0.1.10}
`
	if got := string(readFile(t, filepath.Join(dir, "claude-opus-5-5.yaml"))); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
	if m.Source == nil || m.Source.Catalog != "v0.1.10" {
		t.Fatalf("source = %+v", m.Source)
	}
}

func TestAddBindingChoice(t *testing.T) {
	src := fixtureSource(t, fixtureCatalog("v0.1.10"))
	cases := []struct {
		name, ref, pricedBy string
		wantHost, wantWire  string
	}{
		{"author host", "claude-opus-5-5", "", "anthropic", "claude-opus-5-5"},
		{"author host by wire name", "gpt-5.5-2026-04-23", "", "openai", "gpt-5.5-2026-04-23"},
		{"featured when author hosts nothing", "llama-4-maverick", "", "groq", "llama-4-maverick-17b"},
		{"first when nothing featured", "mistral/mixtral-8x22b", "", "openrouter", "mistralai/mixtral-8x22b"},
		{"pricedBy overrides", "claude-opus-5-5", "openrouter", "openrouter", "anthropic/claude-opus-5-5"},
		{"pinned ref", "claude-opus-5-5@openrouter", "", "openrouter", "anthropic/claude-opus-5-5"},
		{"pricedBy beats pin", "mixtral-8x22b@openrouter", "groq", "groq", "mixtral-8x22b-32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Add(context.Background(), t.TempDir(), tc.ref, src, AddOptions{PricedBy: tc.pricedBy})
			if err != nil {
				t.Fatal(err)
			}
			if m.PricedBy != tc.wantHost || m.WireName != tc.wantWire {
				t.Fatalf("pricedBy=%q wireName=%q, want %q %q", m.PricedBy, m.WireName, tc.wantHost, tc.wantWire)
			}
		})
	}
}

func TestAddErrors(t *testing.T) {
	src := fixtureSource(t, fixtureCatalog("v0.1.10"))
	ctx := context.Background()
	if _, err := Add(ctx, t.TempDir(), "claude-opus-5-5", src, AddOptions{PricedBy: "groq"}); err == nil {
		t.Fatal("pricedBy a host that does not serve the model: want error")
	}
	if _, err := Add(ctx, t.TempDir(), "no-such-model", src, AddOptions{}); err == nil {
		t.Fatal("unknown ref: want error")
	}

	dir := t.TempDir()
	if _, err := Add(ctx, dir, "claude-opus-5-5", src, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(ctx, dir, "claude-opus-5-5", src, AddOptions{PricedBy: "openrouter"}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second add: err = %v, want fs.ErrExist", err)
	}
	m, err := Add(ctx, dir, "claude-opus-5-5", src, AddOptions{PricedBy: "openrouter", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].PricedBy != "openrouter" || m.PricedBy != "openrouter" {
		t.Fatalf("after overwrite: %+v", listed)
	}
}

func TestRemoveAndList(t *testing.T) {
	dir := t.TempDir()
	src := fixtureSource(t, fixtureCatalog("v0.1.10"))
	for _, ref := range []string{"mixtral-8x22b", "claude-opus-5-5", "gpt-5-5-2026-04-23"} {
		if _, err := Add(context.Background(), dir, ref, src, AddOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	writeHandWritten(t, dir)

	if err := Remove(dir, "claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	models, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range models {
		names = append(names, m.Name)
	}
	want := []string{"gpt-5-5-2026-04-23", "local-llm", "mixtral-8x22b"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}

	if err := Remove(dir, "claude-opus-5-5"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("remove twice: err = %v, want fs.ErrNotExist", err)
	}
	for _, bad := range []string{"", "../mixtral-8x22b", "a/b", ".hidden"} {
		if err := Remove(dir, bad); err == nil {
			t.Fatalf("Remove(%q): want error", bad)
		}
	}
}

package graph

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
)

// testCatalog: one model (gpt-x) served by two hosts, authored by acme, with a
// rich sidecar; plus a thin model (loner) that has a binding but no sidecar.
const testCatalog = `{
  "version": "test",
  "hosts": [
    {
      "name": "acme-direct",
      "baseURL": "https://api.acme.example",
      "displayName": "Acme Direct",
      "docsURL": "https://docs.acme.example",
      "icon": "host.svg",
      "models": [
        {"model": "gpt-x", "upstream": "gpt-x-2026", "adapter": "openai", "providers": ["acme"],
         "pricing": [{"meter": "tokens.input", "unit": "per_million", "amount": 1.5}]},
        {"model": "loner", "upstream": "loner-1", "adapter": "anthropic", "providers": ["acme"]}
      ]
    },
    {
      "name": "reseller",
      "baseURL": "https://api.reseller.example",
      "models": [
        {"model": "gpt-x", "upstream": "gpt-x-2026", "adapter": "openai", "providers": ["acme"]}
      ]
    }
  ],
  "models": [
    {"metadataName": "gpt-x", "provider": "acme", "displayName": "GPT-X", "family": "gpt",
     "capabilities": {"chat": true, "tools": true, "vision": true},
     "modalities": {"input": ["text", "image"], "output": ["text"]},
     "contextWindowTotal": 128000, "maxOutputTokens": 8192, "knowledgeCutoff": "2026-01"}
  ],
  "providers": [
    {"name": "acme", "displayName": "Acme AI", "homepageURL": "https://acme.example", "icon": "acme.svg"}
  ]
}`

func build(t *testing.T) *Graph {
	t.Helper()
	ic, err := catalog.LoadBytes([]byte(testCatalog))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return Build(ic)
}

func TestResolveModel_AggregatesHostsAndAuthor(t *testing.T) {
	g := build(t)
	m, err := g.ResolveModel("gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "gpt-x-2026" || m.Slug != "gpt-x" || m.DisplayName != "GPT-X" {
		t.Fatalf("identity wrong: name=%q slug=%q display=%q", m.Name, m.Slug, m.DisplayName)
	}
	if !m.Capabilities.Vision || !m.Capabilities.Tools || !m.Capabilities.Chat {
		t.Fatalf("capabilities not carried: %+v", m.Capabilities)
	}
	if m.ContextWindow.Total != 128000 || m.MaxOutputTokens != 8192 {
		t.Fatalf("context window wrong: %+v / %d", m.ContextWindow, m.MaxOutputTokens)
	}
	if len(m.Modalities.Input) != 2 {
		t.Fatalf("modalities wrong: %+v", m.Modalities)
	}
	if m.Author == nil || m.Author.Name != "acme" || m.Author.DisplayName != "Acme AI" || m.Author.HomepageURL == "" {
		t.Fatalf("author wrong: %+v", m.Author)
	}
	// Two hosts, sorted by name; pricing rides the edge it belongs to.
	if len(m.Hosts) != 2 {
		t.Fatalf("want 2 hosts, got %d", len(m.Hosts))
	}
	if m.Hosts[0].Host.Name != "acme-direct" || m.Hosts[1].Host.Name != "reseller" {
		t.Fatalf("hosts not sorted: %q, %q", m.Hosts[0].Host.Name, m.Hosts[1].Host.Name)
	}
	if len(m.Hosts[0].Pricing) != 1 || m.Hosts[0].Pricing[0].Amount != 1.5 {
		t.Fatalf("edge pricing wrong: %+v", m.Hosts[0].Pricing)
	}
	if len(m.Hosts[1].Pricing) != 0 {
		t.Fatalf("reseller should be unpriced: %+v", m.Hosts[1].Pricing)
	}
	if m.Hosts[0].Host.DisplayName != "Acme Direct" || m.Hosts[0].Host.Icon != "host.svg" {
		t.Fatalf("host metadata not carried: %+v", m.Hosts[0].Host)
	}
}

func TestResolveModel_ByWireName(t *testing.T) {
	g := build(t)
	m, err := g.ResolveModel("gpt-x-2026") // upstream wire name resolves too
	if err != nil {
		t.Fatal(err)
	}
	if m.Slug != "gpt-x" {
		t.Fatalf("wire-name resolve got slug %q", m.Slug)
	}
}

func TestResolveModel_ThinNode(t *testing.T) {
	g := build(t)
	m, err := g.ResolveModel("loner") // binding but no sidecar
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "loner-1" || m.DisplayName != "" {
		t.Fatalf("thin node wrong: name=%q display=%q", m.Name, m.DisplayName)
	}
	if m.Author == nil || m.Author.Name != "acme" {
		t.Fatalf("thin node author (from binding) wrong: %+v", m.Author)
	}
}

func TestResolveHostAndProvider_BackLinks(t *testing.T) {
	g := build(t)
	h, err := g.ResolveHost("acme-direct")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(h.Models, "gpt-x") || !contains(h.Models, "loner") {
		t.Fatalf("host back-links wrong: %v", h.Models)
	}
	p, err := g.ResolveProvider("acme")
	if err != nil {
		t.Fatal(err)
	}
	if p.DisplayName != "Acme AI" || !contains(p.Models, "gpt-x") {
		t.Fatalf("provider node wrong: %+v", p)
	}
}

func TestResolve_Errors(t *testing.T) {
	g := build(t)
	if _, err := g.ResolveModel("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
	if _, err := g.ResolveHost("nope"); err == nil {
		t.Fatal("expected error for unknown host")
	}
	if _, err := g.ResolveProvider("nope"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// The whole point of slug back-links: every node marshals without a cycle.
func TestNodesMarshalAcyclically(t *testing.T) {
	g := build(t)
	m, _ := g.ResolveModel("gpt-x")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal model: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"author"`) || !strings.Contains(s, `"hosts"`) || !strings.Contains(s, `"capabilities"`) {
		t.Fatalf("model json missing nested data: %s", s)
	}
	h, _ := g.ResolveHost("acme-direct")
	if _, err := json.Marshal(h); err != nil {
		t.Fatalf("marshal host: %v", err)
	}
	p, _ := g.ResolveProvider("acme")
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("marshal provider: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

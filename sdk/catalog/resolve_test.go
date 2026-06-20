package catalog

import (
	"encoding/json"
	"testing"
)

func TestResolve_AmbiguousAndPin(t *testing.T) {
	raw := `{
	  "version": "relay-catalog@v1alpha2",
	  "generatedAt": "2026-05-28T00:00:00Z",
	  "hosts": [
	    {
	      "name": "host-a",
	      "baseURL": "https://a.example",
	      "models": [
	        {"model": "gpt-4o", "adapter": "openai", "upstream": "gpt-4o", "providers": ["openai"]}
	      ]
	    },
	    {
	      "name": "host-b",
	      "baseURL": "https://b.example",
	      "models": [
	        {"model": "gpt-4o", "adapter": "openai", "upstream": "gpt-4o", "providers": ["openai"]}
	      ]
	    }
	  ]
	}`
	ic, err := LoadBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ic.Resolve("gpt-4o"); err == nil {
		t.Fatal("expected ambiguous bare ref")
	}
	b, h, err := ic.Resolve("gpt-4o@host-b")
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "host-b" || b.Name != "gpt-4o" {
		t.Fatalf("got host=%q binding=%+v", h.Name, b)
	}
}

func TestResolve_ProviderQualified(t *testing.T) {
	raw := `{
	  "version": "relay-catalog@v1alpha2",
	  "generatedAt": "2026-05-28T00:00:00Z",
	  "hosts": [{
	    "name": "openai",
	    "baseURL": "https://api.openai.com",
	    "models": [{
	      "model": "gpt-4o",
	      "adapter": "openai",
	      "upstream": "gpt-4o-2024-08-06",
	      "providers": ["openai"]
	    }]
	  }]
	}`
	ic, err := LoadBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, h, err := ic.Resolve("openai/gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "gpt-4o-2024-08-06" || h.Name != "openai" {
		t.Fatalf("got host=%q binding=%+v", h.Name, b)
	}
}

// A response's ran-model is the provider's wire spelling, not the catalog
// slug. Every form of it must resolve — the dotted real name (which happens
// to slugify to the catalog key) and wire names whose slugification differs
// from the key entirely (ollama tag syntax).
func TestResolve_ServedWireName(t *testing.T) {
	raw := `{
	  "version": "test",
	  "hosts": [
	    {
	      "name": "openai",
	      "baseURL": "https://api.openai.com",
	      "models": [
	        {"model": "gpt-5-5-2026-04-23", "adapter": "openai", "upstream": "gpt-5.5-2026-04-23", "providers": ["openai"]}
	      ]
	    },
	    {
	      "name": "ollama-cloud",
	      "baseURL": "https://ollama.example",
	      "models": [
	        {"model": "deepseek-v3-1-671b", "adapter": "openai", "upstream": "deepseek-v3.1:671b-cloud", "providers": ["deepseek"]}
	      ]
	    }
	  ]
	}`
	ic, err := LoadBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ref, wantHost, wantUpstream string
	}{
		{"gpt-5.5-2026-04-23", "openai", "gpt-5.5-2026-04-23"},
		{"gpt-5-5-2026-04-23", "openai", "gpt-5.5-2026-04-23"},
		{"openai/gpt-5.5-2026-04-23", "openai", "gpt-5.5-2026-04-23"},
		{"deepseek-v3.1:671b-cloud", "ollama-cloud", "deepseek-v3.1:671b-cloud"},
		{"deepseek-v3-1-671b", "ollama-cloud", "deepseek-v3.1:671b-cloud"},
		{"deepseek-v3.1:671b-cloud@ollama-cloud", "ollama-cloud", "deepseek-v3.1:671b-cloud"},
		{"deepseek/deepseek-v3.1:671b-cloud", "ollama-cloud", "deepseek-v3.1:671b-cloud"},
	}
	for _, tc := range cases {
		b, h, err := ic.Resolve(tc.ref)
		if err != nil {
			t.Errorf("Resolve(%q): %v", tc.ref, err)
			continue
		}
		if h.Name != tc.wantHost || b.Name != tc.wantUpstream {
			t.Errorf("Resolve(%q) = host %q upstream %q, want %q %q", tc.ref, h.Name, b.Name, tc.wantHost, tc.wantUpstream)
		}
	}
}

// A binding whose upstream normalizes to its own catalog key must index once —
// double-indexing would make every single-host bare ref falsely ambiguous.
func TestResolve_UpstreamSameAsModel_NotAmbiguous(t *testing.T) {
	raw := `{
	  "version": "test",
	  "hosts": [{
	    "name": "anthropic",
	    "baseURL": "https://api.anthropic.com",
	    "models": [
	      {"model": "claude-opus-4-7", "adapter": "anthropic", "upstream": "claude-opus-4-7", "providers": ["anthropic"]}
	    ]
	  }]
	}`
	ic, err := LoadBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ic.Resolve("claude-opus-4-7"); err != nil {
		t.Fatalf("bare ref on a single host must resolve: %v", err)
	}
}

func TestLoad_EmbeddedParses(t *testing.T) {
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBytes_Invalid(t *testing.T) {
	if _, err := LoadBytes([]byte("{")); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolve_NotFound(t *testing.T) {
	c := &Catalog{Version: "v", Hosts: nil}
	ic, err := indexCatalog(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ic.Resolve("missing"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	c := &Catalog{
		Version: "relay-catalog@v1alpha2",
		Hosts: []Host{{
			Name: "h", BaseURL: "https://x",
			Models: []Binding{{MetadataName: "m", Adapter: "openai", Name: "m", Providers: []string{"p"}}},
		}},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	ic, err := LoadBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ic.Catalog.Hosts) != 1 {
		t.Fatalf("hosts: %d", len(ic.Catalog.Hosts))
	}
}

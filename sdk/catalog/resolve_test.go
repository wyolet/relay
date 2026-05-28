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
	if h.Name != "host-b" || b.Upstream != "gpt-4o" {
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
	if b.Upstream != "gpt-4o-2024-08-06" || h.Name != "openai" {
		t.Fatalf("got host=%q binding=%+v", h.Name, b)
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
			Models: []Binding{{Model: "m", Adapter: "openai", Upstream: "m", Providers: []string{"p"}}},
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

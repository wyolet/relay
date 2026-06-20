package catalog

import "testing"

func aliasIC(t *testing.T) *IndexedCatalog {
	t.Helper()
	raw := `{
	  "version": "relay-catalog@v1alpha2",
	  "generatedAt": "2026-06-13T00:00:00Z",
	  "hosts": [{
	    "name": "anthropic",
	    "baseURL": "https://api.anthropic.com",
	    "models": [
	      {"model": "claude-fable-5", "adapter": "anthropic", "upstream": "claude-fable-5",
	       "providers": ["anthropic"], "aliases": ["claude-fable-5[1m]", "claude-fable-5x[*]"]},
	      {"model": "claude-fable-5x-real", "adapter": "anthropic", "upstream": "claude-fable-5x-real",
	       "providers": ["anthropic"]}
	    ]
	  }]
	}`
	ic, err := LoadBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return ic
}

func TestResolve_AliasExactForms(t *testing.T) {
	ic := aliasIC(t)
	for _, ref := range []string{
		"claude-fable-5[1m]",
		"CLAUDE-FABLE-5.1M",
		"anthropic/claude-fable-5[1m]",
		"claude-fable-5[1m]@anthropic",
		"anthropic/claude-fable-5[1m]@anthropic",
	} {
		b, h, err := ic.Resolve(ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if b.MetadataName != "claude-fable-5" || h.Name != "anthropic" {
			t.Errorf("Resolve(%q) = %q@%q", ref, b.MetadataName, h.Name)
		}
	}
}

func TestResolve_AliasPattern(t *testing.T) {
	ic := aliasIC(t)
	b, _, err := ic.Resolve("claude-fable-5x[whatever-2027]")
	if err != nil {
		t.Fatal(err)
	}
	if b.MetadataName != "claude-fable-5" {
		t.Errorf("pattern resolved to %q", b.MetadataName)
	}
	// Real catalog names beat the wildcard: "claude-fable-5x-real" is inside
	// the pattern's range but is a real binding key.
	b, _, err = ic.Resolve("claude-fable-5x-real")
	if err != nil {
		t.Fatal(err)
	}
	if b.MetadataName != "claude-fable-5x-real" {
		t.Errorf("real name lost to pattern: %q", b.MetadataName)
	}
	// Pinned refs skip the pattern scan.
	if _, _, err := ic.Resolve("claude-fable-5x[thing]@anthropic"); err == nil {
		t.Error("pinned ref matched a wildcard")
	}
	// Boundary: glued continuation of the literal must not match.
	if _, _, err := ic.Resolve("claude-fable-5xx"); err == nil {
		t.Error("boundary violation: claude-fable-5xx matched claude-fable-5x[*]")
	}
}

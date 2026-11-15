package catalog

import (
	"errors"
	"testing"
)

// resolveFixture covers every way a ref can land: one binding on one host, the same model on two hosts, a wire name that differs from the catalog key, exact and wildcard aliases, and one wire name shared by two models.
const resolveFixture = `{
  "version": "test",
  "hosts": [
    {"name": "openai", "baseURL": "https://api.openai.com", "models": [
      {"model": "gpt-5-5-2026-04-23", "adapter": "openai", "upstream": "gpt-5.5-2026-04-23", "providers": ["openai"]},
      {"model": "gpt-4o", "adapter": "openai", "upstream": "gpt-4o-2024", "providers": ["openai"]}
    ]},
    {"name": "azure", "baseURL": "https://azure.example", "models": [
      {"model": "gpt-4o", "adapter": "openai", "upstream": "gpt-4o-dep", "providers": ["openai"]}
    ]},
    {"name": "ollama-cloud", "baseURL": "https://ollama.example", "models": [
      {"model": "deepseek-v3-1-671b", "adapter": "openai", "upstream": "deepseek-v3.1:671b-cloud", "providers": ["deepseek"]}
    ]},
    {"name": "anthropic", "baseURL": "https://api.anthropic.com", "models": [
      {"model": "claude-fable-5", "adapter": "anthropic", "upstream": "claude-fable-5", "providers": ["anthropic"],
       "aliases": ["claude-fable-5[1m]", "claude-fable-5x[*]", "fable*latest"]},
      {"model": "claude-fable-5x-real", "adapter": "anthropic", "upstream": "claude-fable-5x-real", "providers": ["anthropic"]}
    ]},
    {"name": "reseller-a", "models": [{"model": "model-a", "upstream": "shared", "adapter": "openai", "providers": ["p"]}]},
    {"name": "reseller-b", "models": [{"model": "model-b", "upstream": "shared", "adapter": "openai", "providers": ["p"]}]}
  ]
}`

func TestResolve(t *testing.T) {
	ic, err := LoadBytes([]byte(resolveFixture))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		ref      string
		host     string // "" = expect err
		upstream string
		slug     string // ResolveModelSlug; "" = expect slugErr
		err      string
		slugErr  string
		notFound bool
	}{
		// Catalog key, wire name, provider-qualified and pinned forms of one binding.
		{ref: "gpt-5-5-2026-04-23", host: "openai", upstream: "gpt-5.5-2026-04-23", slug: "gpt-5-5-2026-04-23"},
		{ref: "gpt-5.5-2026-04-23", host: "openai", upstream: "gpt-5.5-2026-04-23", slug: "gpt-5-5-2026-04-23"},
		{ref: "GPT-5.5-2026-04-23", host: "openai", upstream: "gpt-5.5-2026-04-23", slug: "gpt-5-5-2026-04-23"},
		{ref: "openai/gpt-5.5-2026-04-23", host: "openai", upstream: "gpt-5.5-2026-04-23", slug: "gpt-5-5-2026-04-23"},
		{ref: "gpt-5-5-2026-04-23@openai", host: "openai", upstream: "gpt-5.5-2026-04-23", slug: "gpt-5-5-2026-04-23"},
		// A wire name whose slug differs from the catalog key.
		{ref: "deepseek-v3.1:671b-cloud", host: "ollama-cloud", upstream: "deepseek-v3.1:671b-cloud", slug: "deepseek-v3-1-671b"},
		{ref: "deepseek-v3-1-671b", host: "ollama-cloud", upstream: "deepseek-v3.1:671b-cloud", slug: "deepseek-v3-1-671b"},
		{ref: "deepseek-v3.1:671b-cloud@ollama-cloud", host: "ollama-cloud", upstream: "deepseek-v3.1:671b-cloud", slug: "deepseek-v3-1-671b"},
		{ref: "deepseek/deepseek-v3.1:671b-cloud", host: "ollama-cloud", upstream: "deepseek-v3.1:671b-cloud", slug: "deepseek-v3-1-671b"},
		// One model on two hosts: a binding needs a pin, the model slug does not.
		{ref: "gpt-4o", slug: "gpt-4o", err: `catalog: model "gpt-4o" is ambiguous across hosts (gpt-4o@azure, gpt-4o@openai); pin with @host`},
		{ref: "openai/gpt-4o", slug: "gpt-4o", err: `catalog: model "openai-gpt-4o" is ambiguous across hosts (gpt-4o@azure, gpt-4o@openai); pin with @host`},
		{ref: "gpt-4o@azure", host: "azure", upstream: "gpt-4o-dep", slug: "gpt-4o"},
		{ref: "gpt-4o@openai", host: "openai", upstream: "gpt-4o-2024", slug: "gpt-4o"},
		{ref: "openai/gpt-4o@azure", host: "azure", upstream: "gpt-4o-dep", slug: "gpt-4o"},
		// '@', '/' and '-' normalize to the same separator, so the dash form reaches the pin.
		{ref: "gpt-4o-azure", host: "azure", upstream: "gpt-4o-dep", slug: "gpt-4o"},
		// One wire name on two different models is ambiguous for both lookups.
		{ref: "shared", err: `catalog: model "shared" is ambiguous across hosts (model-a@reseller-a, model-b@reseller-b); pin with @host`,
			slugErr: `catalog: ref "shared" maps to multiple models ("model-a", "model-b")`},
		{ref: "shared@reseller-b", host: "reseller-b", upstream: "shared", slug: "model-b"},
		// A binding whose upstream equals its key is indexed once, not as two candidates.
		{ref: "claude-fable-5", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		// Exact aliases, in every form a name takes.
		{ref: "claude-fable-5[1m]", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "CLAUDE-FABLE-5.1M", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "anthropic/claude-fable-5[1m]", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "claude-fable-5[1m]@anthropic", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "anthropic/claude-fable-5[1m]@anthropic", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		// Wildcard aliases: a real name inside the pattern's range wins, a pinned ref never matches, and the literal part ends at a separator.
		{ref: "claude-fable-5x[whatever-2027]", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "claude-fable-5x-real", host: "anthropic", upstream: "claude-fable-5x-real", slug: "claude-fable-5x-real"},
		{ref: "claude-fable-5x[thing]@anthropic", notFound: true},
		{ref: "claude-fable-5xx", notFound: true},
		{ref: "fable-2027-latest", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "fable-latest", host: "anthropic", upstream: "claude-fable-5", slug: "claude-fable-5"},
		{ref: "fable-2027-newest", notFound: true},
		// Misses.
		{ref: "nope", notFound: true},
		{ref: "nope@openai", notFound: true},
		{ref: "gpt-4o@anthropic", notFound: true},
		{ref: "anthropic/gpt-4o", notFound: true},
		{ref: "", err: `catalog: invalid model ref ""`, slugErr: `catalog: invalid model ref ""`},
		{ref: "   ", err: `catalog: invalid model ref "   "`, slugErr: `catalog: invalid model ref "   "`},
		{ref: "@/@", err: `catalog: invalid model ref "@/@"`, slugErr: `catalog: invalid model ref "@/@"`},
	}
	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			if tc.notFound {
				tc.err = `catalog: model "` + tc.ref + `" not found`
				tc.slugErr = tc.err
			}
			b, h, err := ic.Resolve(tc.ref)
			if msg := errText(err); msg != tc.err || h.Name != tc.host || b.Name != tc.upstream {
				t.Errorf("Resolve = upstream %q host %q err %q; want %q %q %q", b.Name, h.Name, msg, tc.upstream, tc.host, tc.err)
			}
			slug, err := ic.ResolveModelSlug(tc.ref)
			if msg := errText(err); msg != tc.slugErr || slug != tc.slug {
				t.Errorf("ResolveModelSlug = %q err %q; want %q %q", slug, msg, tc.slug, tc.slugErr)
			}
			// Only a missing ref is ErrNotFound: an ambiguous or invalid one must not be retried as absent.
			if errors.Is(err, ErrNotFound) != tc.notFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v (err %v)", !tc.notFound, tc.notFound, err)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The longest wildcard prefix wins when two patterns cover a ref.
func TestResolve_LongestWildcardPrefixWins(t *testing.T) {
	ic, err := Index(&Catalog{Hosts: []Host{{Name: "h", Models: []Binding{
		{Name: "short", MetadataName: "short", Aliases: []string{"fam*"}},
		{Name: "long", MetadataName: "long", Aliases: []string{"fam-pro*", "*"}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"fam-pro-2": "long", "fam-lite": "short", "family": "short"} {
		if b, _, err := ic.Resolve(ref); err != nil || b.MetadataName != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, b.MetadataName, err, want)
		}
	}
	// A pattern with nothing before the wildcard is not indexed, so it cannot swallow every ref.
	if _, _, err := ic.Resolve("anything-else"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(anything-else) err = %v, want ErrNotFound", err)
	}
}

func TestIndex(t *testing.T) {
	if ic, err := Index(nil); ic != nil || errText(err) != "catalog: nil catalog" {
		t.Errorf("Index(nil) = %v, %q", ic, errText(err))
	}
	ic, err := Index(&Catalog{Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ic.Resolve("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty catalog: err = %v, want ErrNotFound", err)
	}
}

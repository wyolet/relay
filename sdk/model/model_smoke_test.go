package model_test

import (
	"testing"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/host"
	"github.com/wyolet/relay/sdk/model"
	"github.com/wyolet/relay/sdk/provider"
)

// Resolve a real slug from the embedded catalog through the public package, and
// pin the cross-package alias unification: a model.Model's Author is a
// *provider.Provider and its hosts are *host.Host with no conversion. If these
// types didn't all alias the same internal graph type, this would not compile.
func TestResolveFromEmbeddedCatalog(t *testing.T) {
	ic, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(ic.Catalog.Hosts) == 0 || len(ic.Catalog.Hosts[0].Models) == 0 {
		t.Skip("embedded catalog is empty")
	}
	slug := ic.Catalog.Hosts[0].Models[0].MetadataName

	m, err := model.Resolve(slug)
	if err != nil {
		t.Fatalf("model.Resolve(%q): %v", slug, err)
	}
	if m.Slug != slug {
		t.Fatalf("resolved slug = %q, want %q", m.Slug, slug)
	}
	if len(m.Hosts) == 0 {
		t.Fatalf("model %q resolved with no hosts", slug)
	}

	var _ *host.Host = m.Hosts[0].Host // alias unification (compile-time)
	if m.Author != nil {
		var _ *provider.Provider = m.Author
		if _, err := provider.Resolve(m.Author.Name); err != nil {
			t.Fatalf("provider.Resolve(%q): %v", m.Author.Name, err)
		}
	}
	if _, err := host.Resolve(m.Hosts[0].Host.Name); err != nil {
		t.Fatalf("host.Resolve(%q): %v", m.Hosts[0].Host.Name, err)
	}
}

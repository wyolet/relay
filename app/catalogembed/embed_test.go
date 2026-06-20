package catalogembed

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkcatalog "github.com/wyolet/relay/sdk/catalog"

	"github.com/wyolet/relay/app/manifest"
)

func TestCompose_SkipsDrafts(t *testing.T) {
	dir := t.TempDir()
	prov := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: live
  displayName: Live
spec:
  enabled: true
`
	host := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Host
metadata:
  name: test-host
  displayName: Test Host
spec:
  baseURL: https://example.com
  enabled: true
`
	model := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: live-model
  displayName: Live Model
  owner:
    kind: provider
    id: live
spec:
  enabled: true
  pointer: snap
  snapshots:
    - name: snap
`
	binding := `apiVersion: relay.wyolet.dev/v1alpha2
kind: HostBinding
metadata:
  name: live-model-test-host
  owner:
    kind: system
spec:
  model: live-model
  host: test-host
  adapter: openai
`
	draftModel := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: draft-model
  displayName: Draft
  owner:
    kind: provider
    id: live
spec:
  enabled: true
  pointer: snap
  snapshots:
    - name: snap
`
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "providers", "live", "provider.yaml"), prov)
	write(filepath.Join(dir, "hosts", "test-host", "host.yaml"), host)
	write(filepath.Join(dir, "models", "live-model.yaml"), model)
	write(filepath.Join(dir, "bindings", "live-model-test-host.yaml"), binding)
	write(filepath.Join(dir, "drafts", "models", "draft-model.yaml"), draftModel)

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Compose(docs, time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range cat.Hosts {
		for _, b := range h.Models {
			if b.MetadataName == "draft-model" {
				t.Fatal("draft model appeared in embed")
			}
		}
	}
	found := false
	for _, h := range cat.Hosts {
		for _, b := range h.Models {
			if b.MetadataName == "snap" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected live model binding")
	}
}

func TestCompose_RichSidecars(t *testing.T) {
	dir := t.TempDir()
	prov := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: acme
  displayName: Acme AI
  description: The author
spec:
  homepageURL: https://acme.example
  docsURL: https://docs.acme.example
  icon:
    path: icons/acme.svg
`
	host := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Host
metadata:
  name: acme-direct
  displayName: Acme Direct
spec:
  baseURL: https://api.acme.example
  docsURL: https://docs.acme.example/api
  consoleURL: https://console.acme.example
  icon:
    path: icons/acme-host.svg
`
	model := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: rocket
  displayName: Rocket
  description: fast model
  owner:
    kind: provider
    id: acme
spec:
  family: rocket
  version: "1"
  capabilities:
    chat: true
    tools: true
    vision: true
  modalities:
    input: [text, image]
    output: [text]
  contextWindowTotal: 200000
  maxOutputTokens: 8192
  knowledgeCutoff: "2026-01"
  license: proprietary
  tags: [flagship]
  pointer: rocket-2026
  snapshots:
    - name: rocket-2026
      releasedAt: "2026-03-01"
`
	binding := `apiVersion: relay.wyolet.dev/v1alpha2
kind: HostBinding
metadata:
  name: rocket-acme-direct
  owner:
    kind: system
spec:
  model: rocket
  host: acme-direct
  adapter: openai
`
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "providers", "acme", "provider.yaml"), prov)
	write(filepath.Join(dir, "hosts", "acme-direct", "host.yaml"), host)
	write(filepath.Join(dir, "models", "rocket.yaml"), model)
	write(filepath.Join(dir, "bindings", "rocket-acme-direct.yaml"), binding)

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Compose(docs, time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// Host metadata rides on the host entry.
	if len(cat.Hosts) != 1 {
		t.Fatalf("want 1 host, got %d", len(cat.Hosts))
	}
	h := cat.Hosts[0]
	if h.DisplayName != "Acme Direct" || h.DocsURL == "" || h.ConsoleURL == "" || h.Icon != "icons/acme-host.svg" {
		t.Fatalf("host metadata not surfaced: %+v", h)
	}

	// ModelInfo sidecar keyed by the snapshot slug, carrying family metadata.
	var mi *sdkcatalog.ModelInfo
	for i := range cat.Models {
		if cat.Models[i].MetadataName == "rocket-2026" {
			mi = &cat.Models[i]
		}
	}
	if mi == nil {
		t.Fatalf("rocket-2026 ModelInfo missing; got %+v", cat.Models)
	}
	if mi.Provider != "acme" || mi.DisplayName != "Rocket" || mi.Family != "rocket" {
		t.Fatalf("model identity wrong: %+v", mi)
	}
	if !mi.Capabilities.Vision || !mi.Capabilities.Tools || !mi.Capabilities.Chat {
		t.Fatalf("capabilities not carried: %+v", mi.Capabilities)
	}
	if mi.ContextWindowTotal != 200000 || mi.MaxOutputTokens != 8192 || mi.KnowledgeCutoff != "2026-01" {
		t.Fatalf("numeric metadata wrong: %+v", mi)
	}
	if mi.ReleaseDate != "2026-03-01" { // snapshot date wins over family
		t.Fatalf("release date = %q, want snapshot date", mi.ReleaseDate)
	}
	if len(mi.Modalities.Input) != 2 || mi.Modalities.Output[0] != "text" {
		t.Fatalf("modalities wrong: %+v", mi.Modalities)
	}

	// ProviderInfo sidecar for the author.
	if len(cat.Providers) != 1 {
		t.Fatalf("want 1 provider, got %d", len(cat.Providers))
	}
	p := cat.Providers[0]
	if p.Name != "acme" || p.DisplayName != "Acme AI" || p.HomepageURL == "" || p.Icon != "icons/acme.svg" {
		t.Fatalf("provider metadata not surfaced: %+v", p)
	}
}

func TestValidateAdapters_RejectsUnknownAdapter(t *testing.T) {
	cat := &sdkcatalog.Catalog{
		Hosts: []sdkcatalog.Host{{
			Name: "h",
			Models: []sdkcatalog.Binding{{
				MetadataName: "m", Adapter: "openai_embeddings",
			}},
		}},
	}
	if err := ValidateAdapters(cat); err == nil {
		t.Fatal("expected error for openai_embeddings")
	}
}

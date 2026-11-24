package embed

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdkcatalog "github.com/wyolet/relay/sdk/catalog"

	"github.com/wyolet/relay/pkg/catalogkind/manifest"
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

// The composed document is for direct SDK use: a binding through an upstream adapter the SDK client cannot call drops out, with its host and model sidecar when nothing else serves them; a name that is no upstream adapter stays for ValidateAdapters to reject.
func TestCompose_LeavesOutRelayOnlyAdapters(t *testing.T) {
	for _, tc := range []struct {
		soloAdapter string
		wantHosts   []string
		wantModels  []string
		wantReject  bool
	}{
		{"vertex_anthropic", []string{"host-a"}, []string{"shared-snap"}, false},
		{"no-such-adapter", []string{"host-a", "host-b"}, []string{"shared-snap", "solo-snap"}, true},
	} {
		var docs []manifest.Document
		parse := func(yaml string) {
			t.Helper()
			d, err := manifest.Parse(strings.NewReader(yaml))
			if err != nil {
				t.Fatal(err)
			}
			docs = append(docs, d...)
		}
		parse("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata:\n  name: prov\nspec:\n  enabled: true\n")
		for _, h := range []string{"host-a", "host-b"} {
			parse("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Host\nmetadata:\n  name: " + h + "\nspec:\n  baseURL: https://" + h + ".example\n  enabled: true\n")
		}
		for _, m := range []string{"shared", "solo"} {
			parse("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Model\nmetadata:\n  name: " + m + "\n  owner:\n    kind: provider\n    id: prov\nspec:\n  enabled: true\n  pointer: " + m + "-snap\n  snapshots:\n    - name: " + m + "-snap\n")
		}
		for _, b := range [][3]string{{"shared", "host-a", "openai"}, {"shared", "host-b", "vertex_anthropic"}, {"solo", "host-b", tc.soloAdapter}} {
			parse("apiVersion: relay.wyolet.dev/v1alpha2\nkind: HostBinding\nmetadata:\n  name: " + b[0] + "-" + b[1] + "\n  owner:\n    kind: system\nspec:\n  model: " + b[0] + "\n  host: " + b[1] + "\n  adapter: " + b[2] + "\n")
		}

		cat, err := Compose(docs, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		var hosts, models []string
		for _, h := range cat.Hosts {
			hosts = append(hosts, h.Name)
		}
		for _, m := range cat.Models {
			models = append(models, m.MetadataName)
		}
		slices.Sort(hosts)
		slices.Sort(models)
		if !slices.Equal(hosts, tc.wantHosts) || !slices.Equal(models, tc.wantModels) {
			t.Errorf("%s: hosts %v, models %v; want %v, %v", tc.soloAdapter, hosts, models, tc.wantHosts, tc.wantModels)
		}
		if err := ValidateAdapters(cat); (err != nil) != tc.wantReject {
			t.Errorf("%s: ValidateAdapters = %v, want rejected %v", tc.soloAdapter, err, tc.wantReject)
		}
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
  productLine: booster
  version: "1"
  capabilities:
    chat: true
    tools: true
    vision: true
    reasoning: true
    reasoningEfforts: [low, medium, high, max]
    defaultReasoningEffort: high
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
	if mi.Provider != "acme" || mi.DisplayName != "Rocket" || mi.Family != "rocket" || mi.ProductLine != "booster" {
		t.Fatalf("model identity wrong: %+v", mi)
	}
	if !mi.Capabilities.Vision || !mi.Capabilities.Tools || !mi.Capabilities.Chat {
		t.Fatalf("capabilities not carried: %+v", mi.Capabilities)
	}
	if got := mi.Capabilities.ReasoningEfforts; !slices.Equal(got, []string{"low", "medium", "high", "max"}) || mi.Capabilities.DefaultReasoningEffort != "high" {
		t.Fatalf("reasoning efforts not carried: %v default %q", got, mi.Capabilities.DefaultReasoningEffort)
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

// Host path must survive manifest → embed JSON → SDK load with nil, "" and a
// set value kept distinct, since each means something different to a client.
func TestCompose_HostPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "providers", "acme", "provider.yaml"), `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: acme
spec:
  enabled: true
`)
	write(filepath.Join(dir, "models", "rocket.yaml"), `apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: rocket
  owner:
    kind: provider
    id: acme
spec:
  enabled: true
  pointer: rocket-1
  snapshots:
    - name: rocket-1
`)
	hostPaths := map[string]string{
		"default-path": "",
		"empty-path":   "  path: \"\"\n",
		"custom-path":  "  path: /backend/responses\n",
	}
	for name, pathLine := range hostPaths {
		write(filepath.Join(dir, "hosts", name, "host.yaml"), `apiVersion: relay.wyolet.dev/v1alpha2
kind: Host
metadata:
  name: `+name+`
spec:
  baseURL: https://`+name+`.example
`+pathLine+`  enabled: true
`)
		write(filepath.Join(dir, "bindings", "rocket-"+name+".yaml"), `apiVersion: relay.wyolet.dev/v1alpha2
kind: HostBinding
metadata:
  name: rocket-`+name+`
  owner:
    kind: system
spec:
  model: rocket
  host: `+name+`
  adapter: openai
`)
	}

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	composed, err := Compose(docs, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalJSON(composed)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := sdkcatalog.LoadBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*string{}
	for _, h := range loaded.Catalog.Hosts {
		got[h.Name] = h.Path
	}
	if len(got) != len(hostPaths) {
		t.Fatalf("hosts = %v, want %d", got, len(hostPaths))
	}
	if p := got["default-path"]; p != nil {
		t.Errorf("default-path: Path = %q, want nil", *p)
	}
	if p := got["empty-path"]; p == nil || *p != "" {
		t.Errorf("empty-path: Path = %v, want explicit \"\"", p)
	}
	if p := got["custom-path"]; p == nil || *p != "/backend/responses" {
		t.Errorf("custom-path: Path = %v, want /backend/responses", p)
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

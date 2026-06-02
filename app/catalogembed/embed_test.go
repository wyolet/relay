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
			if b.Model == "draft-model" {
				t.Fatal("draft model appeared in embed")
			}
		}
	}
	found := false
	for _, h := range cat.Hosts {
		for _, b := range h.Models {
			if b.Model == "snap" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected live model binding")
	}
}

func TestValidateAdapters_RejectsUnknownAdapter(t *testing.T) {
	cat := &sdkcatalog.Catalog{
		Hosts: []sdkcatalog.Host{{
			Name: "h",
			Models: []sdkcatalog.Binding{{
				Model: "m", Adapter: "openai_responses",
			}},
		}},
	}
	if err := ValidateAdapters(cat); err == nil {
		t.Fatal("expected error for openai_responses")
	}
}

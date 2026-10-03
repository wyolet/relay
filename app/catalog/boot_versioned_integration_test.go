//go:build integration

package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/app/settings"
)

// stampedTree writes a minimal seedable catalog tree with a .version stamp.
func stampedTree(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	providerDir := filepath.Join(dir, "providers", "acme")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
    name: acme
    displayName: Acme
spec:
    homepageURL: https://acme.test
`
	if err := os.WriteFile(filepath.Join(providerDir, "provider.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".version"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func catalogSourceVersion(t *testing.T, stores *Stores, ctx context.Context) string {
	t.Helper()
	row, err := stores.Settings.Get(ctx, settings.SectionCatalogSource)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	cur, _ := row.Value.(*settings.CatalogSource)
	if cur == nil {
		return ""
	}
	return cur.Version
}

// TestIntegration_SeedVersioned_LocalStampMatch proves a version pin that
// matches the local tree's .version stamp seeds from disk with no
// network: the archive URL is unroutable, so any fetch attempt fails the
// test.
func TestIntegration_SeedVersioned_LocalStampMatch(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	opts := BootstrapOptions{
		Pool:           pool,
		AutoSeedDir:    stampedTree(t, "v9.9.9-test"),
		CatalogVersion: "v9.9.9-test",
		CatalogURL:     "http://127.0.0.1:1/{version}.tar.gz",
	}
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	provs, err := stores.Provider.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 || provs[0].Meta.Name != "acme" {
		t.Fatalf("expected the local tree seeded, got %d providers", len(provs))
	}
	if got := catalogSourceVersion(t, stores, ctx); got != "v9.9.9-test" {
		t.Fatalf("marker = %q, want v9.9.9-test", got)
	}

	// Second hydrate with the same version: marker matches → no-op (would
	// otherwise hit the unroutable URL only if it wrongly re-fetched).
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("second Hydrate: %v", err)
	}
}

// TestIntegration_SeedVersioned_LatestResolvesToLocal proves "latest"
// resolves through the channel index and then still prefers the matching
// local tree over a fetch.
func TestIntegration_SeedVersioned_LatestResolvesToLocal(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	idx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("channels:\n    v1alpha2:\n        latest: v8.8.8-test\n"))
	}))
	defer idx.Close()

	opts := BootstrapOptions{
		Pool:            pool,
		AutoSeedDir:     stampedTree(t, "v8.8.8-test"),
		CatalogVersion:  "latest",
		CatalogURL:      "http://127.0.0.1:1/{version}.tar.gz",
		CatalogIndexURL: idx.URL,
	}
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if got := catalogSourceVersion(t, stores, ctx); got != "v8.8.8-test" {
		t.Fatalf("marker = %q, want v8.8.8-test (resolved via index)", got)
	}
}

// archiveServer serves one catalog archive whose data tree holds files.
func archiveServer(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "relay-catalog/data/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A new catalog release that fails to seed must not take a running relay
// down: the rows already seeded keep serving and the marker stays on the old
// version, so the next boot tries again.
func TestIntegration_SeedVersioned_InvalidReleaseKeepsServingTheExistingCatalog(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	opts := BootstrapOptions{
		Pool:           pool,
		AutoSeedDir:    stampedTree(t, "v1.0.0-test"),
		CatalogVersion: "v1.0.0-test",
		CatalogURL:     "http://127.0.0.1:1/{version}.tar.gz",
	}
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("first Hydrate: %v", err)
	}

	srv := archiveServer(t, map[string]string{
		"providers/broken/provider.yaml": `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
    name: Not A Slug
spec:
    homepageURL: https://broken.test
`,
	})
	opts.CatalogVersion = "v2.0.0-test"
	opts.CatalogURL = srv.URL + "/{version}.tar.gz"
	upgraded, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if _, err := upgraded.Hydrate(ctx, stores, opts); err != nil {
		t.Fatalf("Hydrate with an unseedable release: %v", err)
	}
	if !upgraded.IsReady() {
		t.Fatal("the catalog is not ready after an unseedable release")
	}
	if _, ok := upgraded.Current().ProviderByName("acme"); !ok {
		t.Error("the previously seeded provider is not served")
	}
	if got := catalogSourceVersion(t, stores, ctx); got != "v1.0.0-test" {
		t.Errorf("marker = %q, want it left on v1.0.0-test so the next boot retries", got)
	}
}

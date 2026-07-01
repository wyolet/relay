package seed

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
)

func tarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
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
	return buf.Bytes()
}

func serveArchive(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchCatalog_GitHubArchiveLayout(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"relay-catalog-0.1.0/data/providers/x/provider.yaml": "kind: Provider",
		"relay-catalog-0.1.0/README.md":                      "not data",
	})
	srv := serveArchive(t, archive, http.StatusOK)

	dir := t.TempDir()
	got, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v0.1.0", dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "relay-catalog-0.1.0", "data")
	if got != want {
		t.Fatalf("data root = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, "providers", "x", "provider.yaml")); err != nil {
		t.Fatalf("extracted file missing: %v", err)
	}
}

func TestFetchCatalog_BareDataLayout(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"data/hosts/h/host.yaml": "kind: Host",
	})
	srv := serveArchive(t, archive, http.StatusOK)

	dir := t.TempDir()
	got, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "data"); got != want {
		t.Fatalf("data root = %q, want %q", got, want)
	}
}

func TestFetchCatalog_FlatTreeFallsBackToRoot(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"providers/x/provider.yaml": "kind: Provider",
		"hosts/h/host.yaml":         "kind: Host",
	})
	srv := serveArchive(t, archive, http.StatusOK)

	dir := t.TempDir()
	got, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("data root = %q, want extract root %q", got, dir)
	}
}

func TestFetchCatalog_RejectsTraversal(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"../escape.yaml": "kind: Provider",
	})
	srv := serveArchive(t, archive, http.StatusOK)

	if _, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", t.TempDir()); err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

func TestFetchCatalog_HTTPError(t *testing.T) {
	srv := serveArchive(t, nil, http.StatusNotFound)
	if _, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v9.9.9", t.TempDir()); err == nil {
		t.Fatal("expected HTTP error, got nil")
	}
}

func TestFetchCatalog_RequiresVersion(t *testing.T) {
	if _, err := FetchCatalog(context.Background(), "", "", t.TempDir()); err == nil {
		t.Fatal("expected version-required error, got nil")
	}
}

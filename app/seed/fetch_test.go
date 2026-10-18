package seed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func sha256Line(b []byte) string {
	return fmt.Sprintf("%x  relay-catalog-data.tar.gz\n", sha256.Sum256(b))
}

// serveCatalog serves archive at every path and sum at every path ending in
// ".sha256"; sumStatus other than 200 serves no sum.
func serveCatalog(t *testing.T, archive []byte, sum string, sumStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			w.WriteHeader(sumStatus)
			_, _ = w.Write([]byte(sum))
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serveVerifiedCatalog(t *testing.T, archive []byte) *httptest.Server {
	t.Helper()
	return serveCatalog(t, archive, sha256Line(archive), http.StatusOK)
}

func TestFetchCatalog_ReleaseTarballLayout(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"data/hosts/h/host.yaml": "kind: Host",
	})
	srv := serveVerifiedCatalog(t, archive)

	dir := t.TempDir()
	got, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "data"); got != want {
		t.Fatalf("data root = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, "hosts", "h", "host.yaml")); err != nil {
		t.Fatalf("extracted file missing: %v", err)
	}
}

func TestFetchCatalog_WrappedDataLayout(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"relay-catalog-0.1.0/data/providers/x/provider.yaml": "kind: Provider",
		"relay-catalog-0.1.0/README.md":                      "not data",
	})
	srv := serveVerifiedCatalog(t, archive)

	dir := t.TempDir()
	got, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v0.1.0", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "relay-catalog-0.1.0", "data"); got != want {
		t.Fatalf("data root = %q, want %q", got, want)
	}
}

func TestFetchCatalog_FlatTreeFallsBackToRoot(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"providers/x/provider.yaml": "kind: Provider",
		"hosts/h/host.yaml":         "kind: Host",
	})
	srv := serveVerifiedCatalog(t, archive)

	dir := t.TempDir()
	got, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("data root = %q, want extract root %q", got, dir)
	}
}

func TestFetchCatalog_ChecksumURL(t *testing.T) {
	archive := tarGz(t, map[string]string{"data/hosts/h/host.yaml": "kind: Host"})
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			_, _ = w.Write([]byte(sha256Line(archive)))
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	if _, err := FetchCatalog(context.Background(), srv.URL+"/releases/{version}/data-{version}.tar.gz", "v1", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/releases/v1/data-v1.tar.gz.sha256", "/releases/v1/data-v1.tar.gz"}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("requested %v, want %v", paths, want)
	}
}

func TestFetchCatalog_ChecksumMismatchExtractsNothing(t *testing.T) {
	archive := tarGz(t, map[string]string{"data/hosts/h/host.yaml": "kind: Host"})
	srv := serveCatalog(t, archive, sha256Line([]byte("another archive")), http.StatusOK)

	dir := t.TempDir()
	_, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", dir)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("extracted %d entries from an unverified archive", len(entries))
	}
}

func TestFetchCatalog_MissingChecksum(t *testing.T) {
	archive := tarGz(t, map[string]string{"data/hosts/h/host.yaml": "kind: Host"})
	srv := serveCatalog(t, archive, "", http.StatusNotFound)

	dir := t.TempDir()
	_, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", dir)
	if err == nil || !strings.Contains(err.Error(), ".sha256: HTTP 404") {
		t.Fatalf("err = %v, want the missing checksum reported", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("extracted %d entries without a checksum", len(entries))
	}
}

func TestFetchCatalog_MalformedChecksum(t *testing.T) {
	archive := tarGz(t, map[string]string{"data/hosts/h/host.yaml": "kind: Host"})
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	for name, sum := range map[string]string{
		"empty":     "",
		"blank":     " \n",
		"html":      "<!DOCTYPE html><html></html>",
		"short":     digest[:62] + "  relay-catalog-data.tar.gz\n",
		"long":      digest + "00  relay-catalog-data.tar.gz\n",
		"not hex":   strings.Repeat("z", 64) + "  relay-catalog-data.tar.gz\n",
		"oversized": digest + "  " + strings.Repeat("x", maxSumBytes) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			srv := serveCatalog(t, archive, sum, http.StatusOK)
			if _, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", t.TempDir()); err == nil {
				t.Fatalf("sum %q accepted", sum)
			}
		})
	}
}

func TestFetchCatalog_RejectsTraversal(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"../escape.yaml": "kind: Provider",
	})
	srv := serveVerifiedCatalog(t, archive)

	if _, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", t.TempDir()); err == nil {
		t.Fatal("expected traversal error, got nil")
	}
}

func TestFetchCatalog_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v9.9.9", t.TempDir()); err == nil {
		t.Fatal("expected HTTP error, got nil")
	}
}

func TestFetchCatalog_ArchiveHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			_, _ = w.Write([]byte(sha256Line(nil)))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := FetchCatalog(context.Background(), srv.URL+"/{version}.tar.gz", "v1", t.TempDir()); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("err = %v, want HTTP 503", err)
	}
}

func TestFetchCatalog_RequiresVersion(t *testing.T) {
	if _, err := FetchCatalog(context.Background(), "", "", t.TempDir()); err == nil {
		t.Fatal("expected version-required error, got nil")
	}
}

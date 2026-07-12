package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestChannel(t *testing.T) {
	if got := Channel(); got != "v1alpha2" {
		t.Fatalf("Channel() = %q, want v1alpha2", got)
	}
}

func TestIsLatestAlias(t *testing.T) {
	for v, want := range map[string]bool{"latest": true, "auto": true, "v0.1.1": false, "": false} {
		if IsLatestAlias(v) != want {
			t.Errorf("IsLatestAlias(%q) = %v, want %v", v, !want, want)
		}
	}
}

func TestDirVersion(t *testing.T) {
	dir := t.TempDir()
	if got := DirVersion(dir); got != "" {
		t.Fatalf("unstamped dir: got %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".version"), []byte("v0.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DirVersion(dir); got != "v0.1.1" {
		t.Fatalf("DirVersion = %q, want v0.1.1", got)
	}
	if got := DirVersion(""); got != "" {
		t.Fatalf("empty dir arg: got %q, want empty", got)
	}
}

func TestResolveLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("channels:\n    v1alpha2:\n        latest: v0.1.1\n"))
	}))
	defer srv.Close()

	got, err := ResolveLatest(context.Background(), srv.URL, "v1alpha2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.1.1" {
		t.Fatalf("ResolveLatest = %q, want v0.1.1", got)
	}
}

func TestResolveLatest_UnknownChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("channels:\n    v1alpha2:\n        latest: v0.1.1\n"))
	}))
	defer srv.Close()

	if _, err := ResolveLatest(context.Background(), srv.URL, "v1alpha3"); err == nil {
		t.Fatal("expected error for channel absent from index")
	}
}

func TestResolveLatest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	if _, err := ResolveLatest(context.Background(), srv.URL, "v1alpha2"); err == nil {
		t.Fatal("expected error on HTTP 404")
	}
}

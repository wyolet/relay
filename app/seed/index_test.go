package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChannel(t *testing.T) {
	if got := Channel(); got != "v1alpha3" {
		t.Fatalf("Channel() = %q, want v1alpha3", got)
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

// A known validator goes out as If-None-Match; the server's 304 answers
// NotModified, and a changed index answers its new release and validator.
func TestFetchLatest_ConditionalGET(t *testing.T) {
	etag, latest := `"a"`, "v0.2.1"
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte("channels:\n    v1alpha3:\n        latest: " + latest + "\n"))
	}))
	defer srv.Close()
	ctx := context.Background()

	first, err := FetchLatest(ctx, srv.URL, "v1alpha3", "")
	if err != nil || first.NotModified || first.Latest != "v0.2.1" || first.ETag != `"a"` {
		t.Fatalf("first read = %+v, %v", first, err)
	}
	again, err := FetchLatest(ctx, srv.URL, "v1alpha3", first.ETag)
	if err != nil || !again.NotModified || again.Latest != "" || again.ETag != `"a"` {
		t.Fatalf("unchanged read = %+v, %v", again, err)
	}
	etag, latest = `"b"`, "v0.2.2"
	changed, err := FetchLatest(ctx, srv.URL, "v1alpha3", first.ETag)
	if err != nil || changed.NotModified || changed.Latest != "v0.2.2" || changed.ETag != `"b"` {
		t.Fatalf("changed read = %+v, %v", changed, err)
	}
	if want := []string{"", `"a"`, `"a"`}; strings.Join(sent, ",") != strings.Join(want, ",") {
		t.Fatalf("If-None-Match sent = %q, want %q", sent, want)
	}
}

// A 304 to a request that sent no validator is not a valid answer.
func TestFetchLatest_UnaskedNotModifiedIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	if _, err := FetchLatest(context.Background(), srv.URL, "v1alpha3", ""); err == nil {
		t.Fatal("expected an error")
	}
}

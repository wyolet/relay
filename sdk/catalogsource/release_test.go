package catalogsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/sdk/catalog"
)

// releaseServer serves a channel index and one release's asset, counting hits per path.
type releaseServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newReleaseServer(t *testing.T, tag string, asset []byte, sum string) *releaseServer {
	t.Helper()
	rs := &releaseServer{hits: map[string]int{}}
	files := map[string][]byte{
		"/index.yaml":                                  []byte("channels:\n    v1alpha1:\n        latest: v0.0.9\n    " + Channel + ":\n        latest: " + tag + "\n"),
		"/download/" + tag + "/catalog.json.gz":        asset,
		"/download/" + tag + "/catalog.json.gz.sha256": []byte(sum + "  catalog.json.gz\n"),
	}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.hits[r.URL.Path]++
		rs.mu.Unlock()
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *releaseServer) total() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for _, c := range rs.hits {
		n += c
	}
	return n
}

func (rs *releaseServer) count(path string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.hits[path]
}

func (rs *releaseServer) options() []ReleaseOption {
	return []ReleaseOption{WithIndexURL(rs.URL + "/index.yaml"), WithDownloadURL(rs.URL + "/download/"), WithHTTPClient(rs.Client())}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestReleaseLatestAndCache(t *testing.T) {
	withEmbedded(t, "")
	asset := gzipped(t, catalogJSON(t, fixtureCatalog("relay-catalog@"+Channel)))
	rs := newReleaseServer(t, "v0.1.10", asset, strings.ToUpper(sha256Hex(asset)))
	cache := t.TempDir()
	ctx := context.Background()

	latest := Release("", cache, rs.options()...)
	ic, err := latest.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ic.Catalog.Version != "v0.1.10" {
		t.Fatalf("version = %q, want the tag", ic.Catalog.Version)
	}
	if _, _, err := ic.Resolve("claude-opus-5-5@anthropic"); err != nil {
		t.Fatal(err)
	}
	if rs.count("/index.yaml") != 1 || rs.total() != 3 {
		t.Fatalf("first fetch hits = %d, want index + asset + sha", rs.total())
	}
	if cached := readFile(t, filepath.Join(cache, "v0.1.10", "catalog.json.gz")); sha256Hex(cached) != sha256Hex(asset) {
		t.Fatal("cache holds a different asset")
	}

	pinned, err := Release("v0.1.10", cache, rs.options()...).Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs.total() != 3 || pinned.Catalog.Version != "v0.1.10" {
		t.Fatalf("cached pinned tag: hits = %d, version = %q", rs.total(), pinned.Catalog.Version)
	}

	if _, err := latest.Catalog(ctx); err != nil {
		t.Fatal(err)
	}
	if rs.count("/index.yaml") != 2 || rs.total() != 4 {
		t.Fatalf("latest again: hits = %d, want one more index fetch and no download", rs.total())
	}
}

// withEmbedded stands in an embed that carries tag, for the length of the test.
func withEmbedded(t *testing.T, tag string) {
	t.Helper()
	prevLoad, prevVersion := loadEmbedded, embeddedVersion
	loadEmbedded = func() (*catalog.IndexedCatalog, error) { return catalog.Index(fixtureCatalog(tag)) }
	embeddedVersion = func() string { return tag }
	t.Cleanup(func() { loadEmbedded, embeddedVersion = prevLoad, prevVersion })
}

func TestReleaseServesEmbeddedTag(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	asset := gzipped(t, catalogJSON(t, fixtureCatalog("x")))
	rs := newReleaseServer(t, "v0.1.10", asset, sha256Hex(asset))
	cache := t.TempDir()
	ctx := context.Background()

	ic, err := Release("v0.1.10", cache, rs.options()...).Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rs.total() != 0 || ic.Catalog.Version != "v0.1.10" {
		t.Fatalf("pinned embedded tag: hits = %d, version = %q", rs.total(), ic.Catalog.Version)
	}
	if _, err := os.Stat(filepath.Join(cache, "v0.1.10")); err == nil {
		t.Fatal("embedded tag written to the cache")
	}
	if _, err := Release("", cache, rs.options()...).Catalog(ctx); err != nil {
		t.Fatal(err)
	}
	if rs.count("/index.yaml") != 1 || rs.total() != 1 {
		t.Fatalf("latest == embedded: hits = %d, want only the index", rs.total())
	}
}

func TestReleasePinFailsWhenContextEnds(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(stalled.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	// A tag the embedded catalog can never carry, so the fetch path is exercised.
	_, err := Release("v0.0.0-unembedded", t.TempDir(), WithDownloadURL(stalled.URL), WithHTTPClient(stalled.Client())).Catalog(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "v0.0.0-unembedded") {
		t.Fatalf("err = %v, want a deadline error naming the tag", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %s, want soon after ctx expired", elapsed)
	}
}

func TestReleaseTag(t *testing.T) {
	for version, want := range map[string]string{
		"v0.1.10":                 "v0.1.10",
		"relay-catalog@v1alpha2":  "",
		"":                        "",
		"../v0.1.10":              "",
		"relay-catalog@v0.1.10/x": "",
	} {
		if got := releaseTag(version); got != want {
			t.Errorf("releaseTag(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestReleaseRejectsBadAsset(t *testing.T) {
	withEmbedded(t, "")
	asset := gzipped(t, catalogJSON(t, fixtureCatalog("x")))
	cases := map[string]*releaseServer{
		"sha mismatch":    newReleaseServer(t, "v0.1.10", asset, sha256Hex([]byte("other"))),
		"not a catalog":   newReleaseServer(t, "v0.1.10", []byte("plain"), sha256Hex([]byte("plain"))),
		"unknown release": newReleaseServer(t, "v0.1.9", asset, sha256Hex(asset)),
	}
	for label, rs := range cases {
		t.Run(label, func(t *testing.T) {
			cache := t.TempDir()
			if _, err := Release("v0.1.10", cache, rs.options()...).Catalog(context.Background()); err == nil {
				t.Fatal("want error")
			}
			if _, err := os.Stat(filepath.Join(cache, "v0.1.10")); err == nil {
				t.Fatal("a rejected asset reached the cache")
			}
		})
	}
}

func TestReleaseRejectsUnsafeTag(t *testing.T) {
	rs := newReleaseServer(t, "../escape", nil, "")
	if _, err := Release("", t.TempDir(), rs.options()...).Catalog(context.Background()); err == nil {
		t.Fatal("index tag with a path separator: want error")
	}
	if rs.count("/index.yaml") != 1 || rs.total() != 1 {
		t.Fatalf("hits = %d, want only the index", rs.total())
	}
}

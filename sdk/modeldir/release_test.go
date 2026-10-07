package modeldir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

	m, err := Add(ctx, t.TempDir(), "gpt-5.5-2026-04-23", Release("v0.1.10", cache, rs.options()...), AddOptions{})
	if err != nil || m.Source.Catalog != "v0.1.10" {
		t.Fatalf("Add from release = %+v, %v", m, err)
	}
}

func TestReleaseRejectsBadAsset(t *testing.T) {
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

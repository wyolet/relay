package catalogsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// catalogServer serves the catalog server layout: /index.yaml with an ETag, and /releases/<tag>/ assets. Tests change what it serves between calls.
type catalogServer struct {
	*httptest.Server
	mu          sync.Mutex
	latest      string
	assets      map[string][]byte
	sums        map[string]string
	indexStatus int
	assetStatus int
	hits        map[string]int
	notModified int
}

func newCatalogServer(t *testing.T, latest string, tags ...string) *catalogServer {
	t.Helper()
	cs := &catalogServer{latest: latest, assets: map[string][]byte{}, sums: map[string]string{}, hits: map[string]int{}}
	for _, tag := range tags {
		asset := gzipped(t, catalogJSON(t, fixtureCatalog("x")))
		cs.assets[tag], cs.sums[tag] = asset, sha256Hex(asset)
	}
	cs.Server = httptest.NewServer(http.HandlerFunc(cs.serve))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *catalogServer) serve(w http.ResponseWriter, r *http.Request) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.hits[r.URL.Path]++
	if r.URL.Path == "/index.yaml" {
		if cs.indexStatus != 0 {
			w.WriteHeader(cs.indexStatus)
			return
		}
		body := "channels:\n    v1alpha1:\n        latest: v0.0.9\n"
		if cs.latest != "" {
			body += "    " + Channel + ":\n        latest: " + cs.latest + "\n"
		}
		etag := strconv.Quote(sha256Hex([]byte(body)))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			cs.notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/releases/")
	tag, file, _ := strings.Cut(rest, "/")
	asset, known := cs.assets[tag]
	switch {
	case !ok || !known:
		http.NotFound(w, r)
	case cs.assetStatus != 0:
		w.WriteHeader(cs.assetStatus)
	case file == assetName:
		_, _ = w.Write(asset)
	case file == assetName+".sha256":
		_, _ = w.Write([]byte(cs.sums[tag] + "  " + assetName + "\n"))
	default:
		http.NotFound(w, r)
	}
}

func (cs *catalogServer) set(f func(*catalogServer)) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	f(cs)
}

func (cs *catalogServer) count(path string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.hits[path]
}

func (cs *catalogServer) total() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, c := range cs.hits {
		n += c
	}
	return n
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newLatest builds a Latest with the catalog env vars cleared and a clock the test moves.
func newLatest(t *testing.T, opts ...LatestOption) (*LatestSource, *fakeClock) {
	t.Helper()
	t.Setenv(EnvCatalogURL, "")
	t.Setenv(EnvCatalogCacheDir, "")
	s := Latest(opts...)
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s.now = clock.now
	return s, clock
}

func (cs *catalogServer) options() []LatestOption {
	return []LatestOption{WithBaseURL(cs.URL), WithLatestHTTPClient(cs.Client())}
}

func fetch(t *testing.T, s *LatestSource) *LatestResult {
	t.Helper()
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Catalog == nil {
		t.Fatal("nil catalog")
	}
	return res
}

func wantCurrent(t *testing.T, res *LatestResult, tag string) {
	t.Helper()
	if res.Tag != tag || res.Embedded || res.Stale || res.Err != nil {
		t.Fatalf("result tag %q embedded %v stale %v err %v, want current %s", res.Tag, res.Embedded, res.Stale, res.Err, tag)
	}
}

func TestLatestFetchesThenRevalidatesAfterTheInterval(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	s, clock := newLatest(t, cs.options()...)

	first := fetch(t, s)
	wantCurrent(t, first, "v0.1.11")
	if first.Catalog.Catalog.Version != "v0.1.11" || cs.total() != 3 {
		t.Fatalf("version %q hits %d, want the tag from index + asset + sha", first.Catalog.Catalog.Version, cs.total())
	}

	clock.advance(DefaultRefreshInterval - time.Second)
	wantCurrent(t, fetch(t, s), "v0.1.11")
	if cs.total() != 3 {
		t.Fatalf("hits = %d inside the refresh interval, want none", cs.total())
	}

	clock.advance(2 * time.Second)
	again := fetch(t, s)
	wantCurrent(t, again, "v0.1.11")
	if cs.count("/index.yaml") != 2 || cs.notModified != 1 || cs.total() != 4 {
		t.Fatalf("after the interval: index %d, 304s %d, total %d; want one revalidated index read", cs.count("/index.yaml"), cs.notModified, cs.total())
	}
	if again.Catalog != first.Catalog {
		t.Error("a 304 must keep the held catalog")
	}
}

func TestLatestPicksUpANewTag(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11", "v0.1.12")
	s, clock := newLatest(t, cs.options()...)
	wantCurrent(t, fetch(t, s), "v0.1.11")

	cs.set(func(cs *catalogServer) { cs.latest = "v0.1.12" })
	clock.advance(DefaultRefreshInterval)
	wantCurrent(t, fetch(t, s), "v0.1.12")
	if cs.count("/releases/v0.1.12/catalog.json.gz") != 1 {
		t.Fatal("the new tag was not downloaded")
	}
}

func TestLatestNeverServesAReleaseOlderThanTheEmbed(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.9", "v0.1.9", "main-0123456789ab")
	s, clock := newLatest(t, cs.options()...)

	res := fetch(t, s)
	if !res.Embedded || res.Stale || res.Err != nil || res.Tag != "v0.1.10" {
		t.Fatalf("older index: tag %q embedded %v stale %v err %v, want the embed", res.Tag, res.Embedded, res.Stale, res.Err)
	}
	if cs.count("/releases/v0.1.9/catalog.json.gz") != 0 {
		t.Fatal("an older tag was downloaded")
	}

	// A tag that isn't vX.Y.Z is never compared, so it is served.
	cs.set(func(cs *catalogServer) { cs.latest = "main-0123456789ab" })
	clock.advance(DefaultRefreshInterval)
	wantCurrent(t, fetch(t, s), "main-0123456789ab")
}

func TestLatestTagEqualToTheEmbedIsCurrent(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.10", "v0.1.10")
	s, _ := newLatest(t, cs.options()...)
	wantCurrent(t, fetch(t, s), "v0.1.10")
	if cs.total() != 1 {
		t.Fatalf("hits = %d, want only the index", cs.total())
	}
}

// failTransport fails the test on any request.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected request to %s", r.URL)
	return nil, errors.New("no network")
}

func TestLatestOffMakesNoRequest(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	noNetwork := WithLatestHTTPClient(&http.Client{Transport: failTransport{t}})
	dir := t.TempDir()

	t.Setenv(EnvCatalogURL, "off")
	t.Setenv(EnvCatalogCacheDir, dir)
	for label, s := range map[string]*LatestSource{
		"env":    Latest(noNetwork),
		"option": Latest(WithBaseURL("off"), noNetwork, WithRefreshInterval(0)),
	} {
		for range 2 {
			res, err := s.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !res.Embedded || res.Stale || res.Err != nil || res.Tag != "v0.1.10" {
				t.Fatalf("%s: tag %q embedded %v stale %v err %v", label, res.Tag, res.Embedded, res.Stale, res.Err)
			}
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("off wrote %d entries to the cache dir", len(entries))
	}
}

func TestLatestURLOptionOverridesOff(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	t.Setenv(EnvCatalogURL, "off")
	t.Setenv(EnvCatalogCacheDir, "")
	res, err := Latest(cs.options()...).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent(t, res, "v0.1.11")
}

func TestLatestServerDownServesTheHeldCopy(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	s, clock := newLatest(t, cs.options()...)
	fresh := fetch(t, s)

	cs.set(func(cs *catalogServer) { cs.indexStatus = http.StatusBadGateway })
	clock.advance(DefaultRefreshInterval)
	res := fetch(t, s)
	if res.Catalog != fresh.Catalog || res.Tag != "v0.1.11" || !res.Stale || res.Embedded || res.Err == nil || !strings.Contains(res.Err.Error(), "502") {
		t.Fatalf("tag %q stale %v embedded %v err %v, want the held copy marked stale", res.Tag, res.Stale, res.Embedded, res.Err)
	}

	index := cs.count("/index.yaml")
	clock.advance(DefaultRefreshInterval / 2)
	if again := fetch(t, s); !again.Stale || again.Err == nil {
		t.Fatalf("inside the interval the failed check is served again: %+v", again)
	}
	if cs.count("/index.yaml") != index {
		t.Fatal("a down server was retried inside the refresh interval")
	}

	cs.set(func(cs *catalogServer) { cs.indexStatus = 0 })
	clock.advance(DefaultRefreshInterval)
	wantCurrent(t, fetch(t, s), "v0.1.11")
}

func TestLatestDiskCacheServesAfterARestartWhileDown(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	dir := t.TempDir()
	first, _ := newLatest(t, append(cs.options(), WithCacheDir(dir))...)
	wantCurrent(t, fetch(t, first), "v0.1.11")
	if got := strings.TrimSpace(string(readFile(t, filepath.Join(dir, lastTagFile)))); got != "v0.1.11" {
		t.Fatalf("last tag file = %q", got)
	}
	url := cs.URL
	cs.Close()

	restarted, _ := newLatest(t, WithBaseURL(url), WithCacheDir(dir))
	res := fetch(t, restarted)
	if res.Tag != "v0.1.11" || !res.Stale || res.Embedded || res.Err == nil {
		t.Fatalf("tag %q stale %v embedded %v err %v, want the disk copy marked stale", res.Tag, res.Stale, res.Embedded, res.Err)
	}
	if _, _, err := res.Catalog.Resolve("claude-opus-5-5@anthropic"); err != nil {
		t.Fatal(err)
	}

	bare, _ := newLatest(t, WithBaseURL(url))
	if res := fetch(t, bare); !res.Embedded || res.Stale || res.Err == nil {
		t.Fatalf("nothing held: embedded %v stale %v err %v, want the embed with the cause", res.Embedded, res.Stale, res.Err)
	}
}

func TestLatestUnavailableAssetFallsBack(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			withEmbedded(t, "v0.1.10")
			cs := newCatalogServer(t, "v0.1.11", "v0.1.11", "v0.1.12")
			s, clock := newLatest(t, cs.options()...)
			cs.set(func(cs *catalogServer) { cs.assetStatus = status })
			if res := fetch(t, s); !res.Embedded || res.Err == nil || !strings.Contains(res.Err.Error(), strconv.Itoa(status)) {
				t.Fatalf("nothing held: embedded %v err %v", res.Embedded, res.Err)
			}

			cs.set(func(cs *catalogServer) { cs.assetStatus = 0 })
			clock.advance(DefaultRefreshInterval)
			wantCurrent(t, fetch(t, s), "v0.1.11")

			cs.set(func(cs *catalogServer) { cs.latest, cs.assetStatus = "v0.1.12", status })
			clock.advance(DefaultRefreshInterval)
			if res := fetch(t, s); res.Tag != "v0.1.11" || !res.Stale || res.Err == nil {
				t.Fatalf("new tag unavailable: tag %q stale %v err %v, want the held tag", res.Tag, res.Stale, res.Err)
			}
		})
	}
}

func TestLatestShaMismatchFallsBackAndCachesNothing(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	cs.set(func(cs *catalogServer) { cs.sums["v0.1.11"] = sha256Hex([]byte("other")) })
	dir := t.TempDir()
	s, _ := newLatest(t, append(cs.options(), WithCacheDir(dir))...)
	res := fetch(t, s)
	if !res.Embedded || res.Err == nil || !strings.Contains(res.Err.Error(), "sha256 mismatch") {
		t.Fatalf("embedded %v err %v", res.Embedded, res.Err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a rejected asset left %d entries in the cache dir", len(entries))
	}
}

func TestLatestMissingChannelFallsBack(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "")
	s, _ := newLatest(t, cs.options()...)
	if res := fetch(t, s); !res.Embedded || res.Err == nil || !strings.Contains(res.Err.Error(), Channel) {
		t.Fatalf("embedded %v err %v", res.Embedded, res.Err)
	}
}

func TestLatestReadsTheEnvironment(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	dir := t.TempDir()
	t.Setenv(EnvCatalogURL, cs.URL+"/")
	t.Setenv(EnvCatalogCacheDir, dir)

	res, err := Latest(WithLatestHTTPClient(cs.Client())).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent(t, res, "v0.1.11")
	if cs.count("/index.yaml") != 1 || cs.count("/releases/v0.1.11/catalog.json.gz") != 1 || cs.count("/releases/v0.1.11/catalog.json.gz.sha256") != 1 {
		t.Fatalf("hits = %v, want index.yaml and releases/<tag>/ under the base", cs.hits)
	}
	if _, err := os.Stat(filepath.Join(dir, "v0.1.11", assetName)); err != nil {
		t.Fatalf("WR_CATALOG_CACHE_DIR not used: %v", err)
	}

	other := t.TempDir()
	t.Setenv(EnvCatalogCacheDir, other)
	if _, err := Latest(WithLatestHTTPClient(cs.Client()), WithCacheDir("")).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatal("WithCacheDir(\"\") must override WR_CATALOG_CACHE_DIR")
	}
}

func TestLatestConcurrentFetch(t *testing.T) {
	withEmbedded(t, "v0.1.10")
	cs := newCatalogServer(t, "v0.1.11", "v0.1.11")
	s, _ := newLatest(t, cs.options()...)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := s.Fetch(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if cs.total() != 3 {
		t.Fatalf("hits = %d, want one fetch shared by every caller", cs.total())
	}
}

func TestOlderRelease(t *testing.T) {
	for _, c := range []struct {
		tag, base string
		want      bool
	}{
		{"v0.1.9", "v0.1.10", true},
		{"v0.1.10", "v0.1.10", false},
		{"v0.2.0", "v0.1.10", false},
		{"v0.9.99", "v1.0.0", true},
		{"main-0123456789ab", "v0.1.10", false},
		{"v0.1.9", "", false},
		{"v0.1.9", "relay-catalog@v1alpha2", false},
		{"v0.1", "v0.1.10", false},
		{"v0.1.+9", "v0.1.10", false},
	} {
		if got := olderRelease(c.tag, c.base); got != c.want {
			t.Errorf("olderRelease(%q, %q) = %v, want %v", c.tag, c.base, got, c.want)
		}
	}
}

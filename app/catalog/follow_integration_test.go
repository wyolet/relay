//go:build integration

package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/metrics"
)

// releaseServer is a catalog server: a channel index with an ETag, and
// release archives with their sha256 files.
type releaseServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	latest   string
	archives map[string][]byte
	badSum   map[string]bool
	fetches  map[string]int
	sentETag []string
	delay    time.Duration
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{t: t, archives: map[string][]byte{}, badSum: map[string]bool{}, fetches: map[string]int{}}
	rs.srv = httptest.NewServer(http.HandlerFunc(rs.serve))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *releaseServer) serve(w http.ResponseWriter, r *http.Request) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r.URL.Path == "/index.yaml" {
		rs.sentETag = append(rs.sentETag, r.Header.Get("If-None-Match"))
		etag := `"` + rs.latest + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		fmt.Fprintf(w, "channels:\n    v1alpha3:\n        latest: %s\n", rs.latest)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/releases/")
	version, isSum := strings.CutSuffix(name, ".tar.gz.sha256")
	if !isSum {
		version = strings.TrimSuffix(name, ".tar.gz")
	}
	archive, ok := rs.archives[version]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if isSum {
		sum := sha256.Sum256(archive)
		if rs.badSum[version] {
			sum = sha256.Sum256([]byte("tampered"))
		}
		fmt.Fprintf(w, "%x  relay-catalog-data.tar.gz\n", sum)
		return
	}
	rs.fetches[version]++
	if rs.delay > 0 {
		rs.mu.Unlock()
		time.Sleep(rs.delay)
		rs.mu.Lock()
	}
	_, _ = w.Write(archive)
}

// release publishes version with one provider per name and makes it latest.
func (rs *releaseServer) release(version string, providers ...string) {
	rs.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, p := range providers {
		body := fmt.Sprintf("apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata:\n    name: %s\nspec:\n    homepageURL: https://%s.test\n", p, p)
		if err := tw.WriteHeader(&tar.Header{Name: "data/providers/" + p + "/provider.yaml", Mode: 0o644, Size: int64(len(body))}); err != nil {
			rs.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			rs.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		rs.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		rs.t.Fatal(err)
	}
	rs.mu.Lock()
	rs.archives[version] = buf.Bytes()
	rs.latest = version
	rs.mu.Unlock()
}

func (rs *releaseServer) setLatest(v string) {
	rs.mu.Lock()
	rs.latest = v
	rs.mu.Unlock()
}

func (rs *releaseServer) fetchCount(v string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.fetches[v]
}

func (rs *releaseServer) indexReads() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.sentETag)
}

func (rs *releaseServer) lastSentETag() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.sentETag[len(rs.sentETag)-1]
}

func (rs *releaseServer) opts(pool *pgxpool.Pool, version string) BootstrapOptions {
	return BootstrapOptions{
		Pool:            pool,
		CatalogVersion:  version,
		CatalogIndexURL: rs.srv.URL + "/index.yaml",
		CatalogURL:      rs.srv.URL + "/releases/{version}.tar.gz",
		SeedLock: func(ctx context.Context, fn func(context.Context) error) error {
			return storage.WithAdvisoryLock(ctx, pool, catalogSeedLock, fn)
		},
		SeedTryLock: func(ctx context.Context, fn func(context.Context) error) (bool, error) {
			return storage.TryAdvisoryLock(ctx, pool, catalogSeedLock, fn)
		},
	}
}

func hydrate(t *testing.T, ctx context.Context, opts BootstrapOptions) (*Catalog, *Stores, *Listener) {
	t.Helper()
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	l, err := cat.Hydrate(ctx, stores, opts)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	return cat, stores, l
}

func catalogSource(t *testing.T, ctx context.Context, stores *Stores) *settings.CatalogSource {
	t.Helper()
	cur, err := readCatalogSource(ctx, stores)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

func providerNames(t *testing.T, ctx context.Context, stores *Stores) string {
	t.Helper()
	provs, err := stores.Provider.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(provs))
	for _, p := range provs {
		if p.Meta.CatalogDetachedAt == nil {
			names = append(names, p.Meta.Name)
		}
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// An unchanged index costs a conditional GET answered 304 and seeds nothing.
func TestIntegration_Follower_UnchangedIndexSeedsNothing(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	opts := rs.opts(pool, "latest")
	_, stores, _ := hydrate(t, ctx, opts)
	f := NewFollower(stores, opts)

	for i := 0; i < 2; i++ {
		res, err := f.Check(ctx)
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if res.Changed || res.Previous != "v1.0.0" || res.Current != "v1.0.0" {
			t.Fatalf("check %d = %+v", i, res)
		}
	}
	if got := rs.lastSentETag(); got != `"v1.0.0"` {
		t.Fatalf("second check sent If-None-Match %q, want the first answer's ETag", got)
	}
	if n := rs.fetchCount("v1.0.0"); n != 1 {
		t.Fatalf("v1.0.0 fetched %d times, want once (at boot)", n)
	}
	if cur := catalogSource(t, ctx, stores); cur.CheckedAt == "" || cur.LastError != "" || cur.Version != "v1.0.0" {
		t.Fatalf("catalog-source = %+v", cur)
	}
}

// A newer channel release is fetched, seeded and marked once.
func TestIntegration_Follower_SeedsANewRelease(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	opts := rs.opts(pool, "latest")
	_, stores, _ := hydrate(t, ctx, opts)
	f := NewFollower(stores, opts)

	rs.release("v1.1.0", "acme", "beta")
	res, err := f.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Previous != "v1.0.0" || res.Current != "v1.1.0" {
		t.Fatalf("check = %+v", res)
	}
	if got := providerNames(t, ctx, stores); got != "acme,beta" {
		t.Fatalf("providers = %s", got)
	}
	if res, err := f.Check(ctx); err != nil || res.Changed {
		t.Fatalf("second check = %+v, %v", res, err)
	}
	if n := rs.fetchCount("v1.1.0"); n != 1 {
		t.Fatalf("v1.1.0 fetched %d times, want 1", n)
	}
	if cur := catalogSource(t, ctx, stores); cur.Version != "v1.1.0" || cur.SeededAt == "" {
		t.Fatalf("catalog-source = %+v", cur)
	}
}

// A webhook for a release the index doesn't list yet, as when it is read from
// a catalog server replica that hasn't synced, is seeded by the one re-check.
func TestIntegration_Follower_RecheckSeedsALaggingRelease(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	opts := rs.opts(pool, "latest")
	_, stores, _ := hydrate(t, ctx, opts)
	rs.release("v1.1.0", "acme", "beta")
	rs.setLatest("v1.0.0")
	reads := rs.indexReads()

	f := NewFollower(stores, opts)
	f.recheckDelay = 100 * time.Millisecond
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.Run(runCtx, 0); close(done) }()
	defer func() { stop(); <-done }()

	f.Nudge("v1.1.0")
	deadline := time.Now().Add(5 * time.Second)
	for rs.indexReads() < reads+1 {
		if time.Now().After(deadline) {
			t.Fatal("the nudge started no check")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rs.setLatest("v1.1.0")
	for catalogSource(t, ctx, stores).Version != "v1.1.0" {
		if time.Now().After(deadline) {
			t.Fatal("the re-check never seeded v1.1.0")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(3 * f.recheckDelay)
	if n := rs.indexReads() - reads; n != 2 {
		t.Fatalf("index reads = %d, want 2 (the check and one re-check)", n)
	}
	if n := rs.fetchCount("v1.1.0"); n != 1 {
		t.Fatalf("v1.1.0 fetched %d times, want 1", n)
	}
	if got := providerNames(t, ctx, stores); got != "acme,beta" {
		t.Fatalf("providers = %s", got)
	}
}

// Two relays on one database checking together: one seeds, the other skips
// on the lock or finds the release already seeded.
func TestIntegration_Follower_TwoRelaysSeedOnce(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	opts := rs.opts(pool, "latest")
	_, storesA, _ := hydrate(t, ctx, opts)
	_, storesB, _ := hydrate(t, ctx, opts)
	followers := []*Follower{NewFollower(storesA, opts), NewFollower(storesB, opts)}

	rs.release("v1.1.0", "acme", "beta")
	rs.mu.Lock()
	rs.delay = 300 * time.Millisecond
	rs.mu.Unlock()
	results := make([]CheckResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, f := range followers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.Check(ctx)
		}()
	}
	wg.Wait()

	changed := 0
	for i := range followers {
		switch {
		case errs[i] == nil && results[i].Changed:
			changed++
		case errs[i] == nil, errors.Is(errs[i], ErrSeedBusy):
		default:
			t.Fatalf("relay %d: %v", i, errs[i])
		}
	}
	if changed != 1 {
		t.Fatalf("relays that seeded = %d, want 1 (results %+v, errs %v)", changed, results, errs)
	}
	if n := rs.fetchCount("v1.1.0"); n != 1 {
		t.Fatalf("v1.1.0 fetched %d times, want 1", n)
	}
	if v := catalogSource(t, ctx, storesA).Version; v != "v1.1.0" {
		t.Fatalf("marker = %s", v)
	}
}

// A channel index naming a release older than the seeded one never moves the
// catalog back.
func TestIntegration_Follower_NeverSeedsOlder(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	rs.release("v1.1.0", "acme", "beta")
	opts := rs.opts(pool, "latest")
	_, stores, _ := hydrate(t, ctx, opts)

	rs.setLatest("v1.0.0")
	res, err := NewFollower(stores, opts).Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Current != "v1.1.0" {
		t.Fatalf("check = %+v", res)
	}
	if n := rs.fetchCount("v1.0.0"); n != 0 {
		t.Fatalf("v1.0.0 fetched %d times, want 0", n)
	}
	if got := providerNames(t, ctx, stores); got != "acme,beta" {
		t.Fatalf("providers = %s", got)
	}
}

// An exact pin older than the seeded release is refused at boot too, unless
// RELAY_CATALOG_ALLOW_DOWNGRADE is set; then it seeds once.
func TestIntegration_Boot_OlderPin(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	rs.release("v1.1.0", "acme", "beta")
	_, stores, _ := hydrate(t, ctx, rs.opts(pool, "v1.1.0"))

	hydrate(t, ctx, rs.opts(pool, "v1.0.0"))
	if v := catalogSource(t, ctx, stores).Version; v != "v1.1.0" {
		t.Fatalf("an older pin moved the marker to %s", v)
	}
	if n := rs.fetchCount("v1.0.0"); n != 0 {
		t.Fatalf("v1.0.0 fetched %d times, want 0", n)
	}

	downgrade := rs.opts(pool, "v1.0.0")
	downgrade.AllowDowngrade = true
	hydrate(t, ctx, downgrade)
	hydrate(t, ctx, downgrade)
	if v := catalogSource(t, ctx, stores).Version; v != "v1.0.0" {
		t.Fatalf("marker = %s, want v1.0.0 after the allowed downgrade", v)
	}
	if n := rs.fetchCount("v1.0.0"); n != 1 {
		t.Fatalf("v1.0.0 fetched %d times, want 1", n)
	}
	if got := providerNames(t, ctx, stores); got != "acme" {
		t.Fatalf("providers = %s, want beta detached", got)
	}
}

// A release whose sum doesn't match keeps the seeded catalog, records the
// error, and is retried in full (no If-None-Match) until it verifies.
func TestIntegration_Follower_FailedFetchKeepsTheCatalog(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	opts := rs.opts(pool, "latest")
	_, stores, _ := hydrate(t, ctx, opts)
	f := NewFollower(stores, opts)

	rs.release("v1.1.0", "acme", "beta")
	rs.mu.Lock()
	rs.badSum["v1.1.0"] = true
	rs.mu.Unlock()
	if _, err := f.Check(ctx); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want the sha256 mismatch", err)
	}
	cur := catalogSource(t, ctx, stores)
	if cur.Version != "v1.0.0" || !strings.Contains(cur.LastError, "sha256") || cur.CheckedAt == "" {
		t.Fatalf("catalog-source = %+v", cur)
	}
	if got := providerNames(t, ctx, stores); got != "acme" {
		t.Fatalf("providers = %s", got)
	}

	rs.mu.Lock()
	rs.badSum["v1.1.0"] = false
	rs.mu.Unlock()
	res, err := f.Check(ctx)
	if err != nil || !res.Changed {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if got := rs.lastSentETag(); got != "" {
		t.Fatalf("retry sent If-None-Match %q after a failed check", got)
	}
	if cur := catalogSource(t, ctx, stores); cur.Version != "v1.1.0" || cur.LastError != "" {
		t.Fatalf("catalog-source = %+v", cur)
	}
}

// A follower's seed reaches another pod's snapshot through NOTIFY, with no
// reload of its own.
func TestIntegration_Follower_SeedReachesOtherPods(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()
	rs := newReleaseServer(t)
	rs.release("v1.0.0", "acme")
	opts := rs.opts(pool, "latest")
	_, storesA, _ := hydrate(t, ctx, opts)
	catB, _, listenerB := hydrate(t, ctx, opts)
	runListener(t, ctx, catB, listenerB)

	rs.release("v1.1.0", "acme", "beta")
	if res, err := NewFollower(storesA, opts).Check(ctx); err != nil || !res.Changed {
		t.Fatalf("check = %+v, %v", res, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, hasBeta := catB.Current().ProviderByName("beta")
		v, _ := catB.Setting(settings.SectionCatalogSource)
		if cs, _ := v.(*settings.CatalogSource); hasBeta && cs != nil && cs.Version == "v1.1.0" {
			if got := testutil.ToFloat64(metrics.CatalogSeededVersion.WithLabelValues("v1.1.0")); got != 1 {
				t.Fatalf("relay_catalog_seeded_version{version=v1.1.0} = %v, want 1", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the other pod's snapshot never saw the seeded release")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

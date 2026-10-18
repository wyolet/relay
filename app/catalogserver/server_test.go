package catalogserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBuild_ExcludesDraftsAndReadsCommitTime(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", true)

	repo := Repo{Dir: src.dir}
	commit, at, err := repo.Commit(context.Background(), "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !at.Equal(oldCommit) {
		t.Fatalf("commit time %v, want %v", at, oldCommit)
	}
	if _, off := at.Zone(); off != 5*3600 {
		t.Fatalf("commit time offset %d, want the committer's +05:00", off)
	}
	data, err := BuildData(context.Background(), repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := ArchiveChannel(data)
	if err != nil || channel != "v1alpha2" {
		t.Fatalf("channel %q, %v; drafts must not reach the archive", channel, err)
	}
	again, err := BuildData(context.Background(), repo, commit)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatal("data tarball is not reproducible")
	}
	cat1, err := BuildCatalog(data, at, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	cat2, _ := BuildCatalog(data, at, "v0.1.0")
	if !bytes.Equal(cat1, cat2) {
		t.Fatal("catalog is not reproducible")
	}
	if got := string(SumFile(CatalogAsset, cat1)); !strings.HasSuffix(got, "  catalog.json.gz\n") || len(got) != 64+2+len(CatalogAsset)+1 {
		t.Fatalf("sum file %q is not sha256sum format", got)
	}
}

func TestArchiveChannel_DocumentsMustAgree(t *testing.T) {
	src := newSourceRepo(t)
	tree := catalogTree(fixtureAPIVersion, "Rocket")
	tree["data/hosts/acme-api/host.yaml"] = strings.Replace(tree["data/hosts/acme-api/host.yaml"],
		"apiVersion: "+fixtureAPIVersion+"\nkind: HostBinding", "apiVersion: relay.wyolet.dev/v1beta1\nkind: HostBinding", 1)
	commit := src.commit(tree, oldCommit)
	data, err := BuildData(context.Background(), Repo{Dir: src.dir}, commit)
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := ArchiveChannel(data); err == nil {
		t.Fatalf("mixed apiVersions gave channel %q", ch)
	}
}

func TestServe_RebuildMatchingPublishedIsServed(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", true)
	rel := newFakeReleases(t)
	data, cat := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data, cat)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if got := indexLatest(t, s); got["v1alpha2"] != "v0.1.0" || len(got) != 1 {
		t.Fatalf("index %v", got)
	}
	for name, want := range map[string][]byte{CatalogAsset: cat, DataAsset("v0.1.0"): data} {
		rec := get(t, s, "/releases/v0.1.0/"+name)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("%s: %d, body equal %v", name, rec.Code, bytes.Equal(rec.Body.Bytes(), want))
		}
		if n := rel.fileHits("v0.1.0", name); n != 0 {
			t.Fatalf("%s fetched from the release %d times; the rebuild matched", name, n)
		}
	}
	if n := testutil.ToFloat64(s.metrics.mismatches.WithLabelValues("catalog")); n != 0 {
		t.Fatalf("mismatches %v", n)
	}
	if rec := get(t, s, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("readyz %d", rec.Code)
	}
}

func TestServe_MismatchServesPublishedFile(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	rel := newFakeReleases(t)
	data, _ := src.releaseFiles("v0.1.0")
	// A catalog an older builder produced: valid, but not what this one builds.
	published, err := BuildCatalog(data, oldCommit.Add(time.Hour), "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	rel.publishBoth("v0.1.0", data, published)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if got := indexLatest(t, s); got["v1alpha2"] != "v0.1.0" {
		t.Fatalf("index %v", got)
	}
	rec := get(t, s, "/releases/v0.1.0/catalog.json.gz")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), published) {
		t.Fatalf("served %d, published bytes %v", rec.Code, bytes.Equal(rec.Body.Bytes(), published))
	}
	if n := testutil.ToFloat64(s.metrics.mismatches.WithLabelValues("catalog")); n != 1 {
		t.Fatalf("catalog mismatches %v, want 1", n)
	}
	if n := testutil.ToFloat64(s.metrics.mismatches.WithLabelValues("data")); n != 0 {
		t.Fatalf("data mismatches %v, want 0", n)
	}
	if n := rel.fileHits("v0.1.0", CatalogAsset); n != 1 {
		t.Fatalf("published catalog fetched %d times, want 1", n)
	}
}

func TestServe_BadPublishedFileIsNeverServedOrIndexed(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	data1, _ := src.releaseFiles("v0.1.1")
	rel.publish("v0.1.1", DataAsset("v0.1.1"), data1, data1)
	rel.publish("v0.1.1", CatalogAsset, gzipBytes(t, "tampered"), gzipBytes(t, "what CI built"))

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if got := indexLatest(t, s); got["v1alpha2"] != "v0.1.0" {
		t.Fatalf("index %v, want v0.1.0: v0.1.1's catalog never verifies", got)
	}
	if rec := get(t, s, "/releases/v0.1.1/catalog.json.gz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bad catalog: %d, want 503", rec.Code)
	}
	if rec := get(t, s, "/releases/v0.1.1/"+DataAsset("v0.1.1")); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data1) {
		t.Fatalf("good data file of the same tag: %d", rec.Code)
	}
}

func TestIndex_WaitsForBothSums(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), time.Now().Add(-time.Hour).Truncate(time.Second))
	src.tag("v0.1.1", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publish("v0.1.1", CatalogAsset, cat1, cat1)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())
	if got := indexLatest(t, s); got["v1alpha2"] != "v0.1.0" {
		t.Fatalf("index %v before the data sum is published", got)
	}
	if rec := get(t, s, "/releases/v0.1.1/"+DataAsset("v0.1.1")); rec.Code != http.StatusNotFound {
		t.Fatalf("unpublished data file: %d, want 404", rec.Code)
	}

	rel.publish("v0.1.1", DataAsset("v0.1.1"), data1, data1)
	s.sync(context.Background())
	if got := indexLatest(t, s); got["v1alpha2"] != "v0.1.1" {
		t.Fatalf("index %v after both sums are published", got)
	}
}

func TestIndex_UnknownChannelServesPublishedFiles(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree("relay.wyolet.dev/v1beta1", "Rocket"), newCommit)
	src.tag("v0.2.0", false)
	src.tag("v0.3.0-rc.1", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	commit, _, err := Repo{Dir: src.dir}.Commit(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	data2, err := BuildData(context.Background(), Repo{Dir: src.dir}, commit)
	if err != nil {
		t.Fatal(err)
	}
	cat2 := gzipBytes(t, `{"version":"v0.2.0","schema":"a future one"}`)
	rel.publishBoth("v0.2.0", data2, cat2)
	rel.publishBoth("v0.3.0-rc.1", data2, cat2)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	got := indexLatest(t, s)
	if got["v1alpha2"] != "v0.1.0" || got["v1beta1"] != "v0.2.0" || len(got) != 2 {
		t.Fatalf("index %v", got)
	}
	rec := get(t, s, "/releases/v0.2.0/catalog.json.gz")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cat2) {
		t.Fatalf("published catalog of an unknown schema: %d", rec.Code)
	}
	if n := testutil.ToFloat64(s.metrics.buildFailures.WithLabelValues("catalog")); n != 1 {
		t.Fatalf("catalog build failures %v, want 1", n)
	}
	if rec := get(t, s, "/releases/v0.3.0-rc.1/catalog.json.gz"); rec.Code != http.StatusNotFound {
		t.Fatalf("pre-release tag: %d, want 404", rec.Code)
	}
	want := "channels:\n    v1alpha2:\n        latest: v0.1.0\n    v1beta1:\n        latest: v0.2.0\n"
	if body := get(t, s, "/index.yaml").Body.String(); body != want {
		t.Fatalf("index.yaml:\n%s\nwant:\n%s", body, want)
	}
}

func TestHTTP_HeadersAndConditionalRequests(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	rel := newFakeReleases(t)
	data, cat := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data, cat)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	idx := get(t, s, "/index.yaml")
	etag := idx.Header().Get("ETag")
	if idx.Code != http.StatusOK || idx.Header().Get("Cache-Control") != "public, max-age=60" || etag != `"`+sha256Hex(idx.Body.Bytes())+`"` {
		t.Fatalf("index: %d %v", idx.Code, idx.Header())
	}
	if rec := get(t, s, "/index.yaml", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("index revalidation: %d, want 304", rec.Code)
	}

	rec := get(t, s, "/releases/v0.1.0/catalog.json.gz", "Accept-Encoding", "gzip")
	h := rec.Header()
	if rec.Code != http.StatusOK || h.Get("Content-Type") != "application/gzip" || h.Get("Content-Encoding") != "" ||
		h.Get("Cache-Control") != "public, max-age=31536000, immutable" || h.Get("ETag") != `"`+sha256Hex(cat)+`"` {
		t.Fatalf("catalog: %d %v", rec.Code, h)
	}
	if rec := get(t, s, "/releases/v0.1.0/catalog.json.gz", "If-None-Match", h.Get("ETag")); rec.Code != http.StatusNotModified {
		t.Fatalf("catalog revalidation: %d, want 304", rec.Code)
	}
	sum := get(t, s, "/releases/v0.1.0/catalog.json.gz.sha256")
	if sum.Code != http.StatusOK || sum.Body.String() != string(SumFile(CatalogAsset, cat)) {
		t.Fatalf("sum: %d %q", sum.Code, sum.Body.String())
	}

	for path, want := range map[string]string{
		"/channels/v1alpha2/latest/catalog.json.gz":                  "/releases/v0.1.0/catalog.json.gz",
		"/channels/v1alpha2/latest/relay-catalog-data.tar.gz.sha256": "/releases/v0.1.0/relay-catalog-data-v0.1.0.tar.gz.sha256",
	} {
		rec := get(t, s, path)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want || rec.Header().Get("Cache-Control") != "public, max-age=60" {
			t.Fatalf("%s: %d %v", path, rec.Code, rec.Header())
		}
	}
}

func TestHTTP_NotFound(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	rel := newFakeReleases(t)
	data, cat := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data, cat)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	for _, path := range []string{
		"/releases/v9.9.9/catalog.json.gz",
		"/releases/latest/catalog.json.gz",
		"/releases/v0.1/catalog.json.gz",
		"/releases/v0.1.0+meta/catalog.json.gz",
		"/releases/..%2Fv0.1.0/catalog.json.gz",
		"/releases/v0.1.0/catalog.json",
		"/releases/v0.1.0/relay-catalog-data-v0.1.1.tar.gz",
		"/releases/v0.1.0/catalog.json.gz.sha256.sha256",
		"/releases/v0.1.1/catalog.json.gz",
		"/releases/v0.1.1/catalog.json.gz.sha256",
		"/channels/v9/latest/catalog.json.gz",
		"/channels/v1alpha2/latest/index.yaml",
	} {
		if rec := get(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rec.Code)
		}
	}
	if n := testutil.ToFloat64(s.metrics.builds.WithLabelValues("catalog")); n != 1 {
		t.Fatalf("catalog builds %v, want 1 (v0.1.0 only)", n)
	}
}

func TestUnknownTag_TriggersOneExtraFetch(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	s := newTestServer(t, src, rel, 64<<20)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	triggered := func() bool {
		select {
		case <-s.fetchNow:
			return true
		default:
			return false
		}
	}
	get(t, s, "/releases/v1.0.0/catalog.json.gz")
	if !triggered() {
		t.Fatal("first miss did not trigger a fetch")
	}
	get(t, s, "/releases/v1.0.0/catalog.json.gz")
	get(t, s, "/releases/v1.0.1/catalog.json.gz")
	if triggered() {
		t.Fatal("misses within 10s triggered another fetch")
	}
	now = now.Add(11 * time.Second)
	get(t, s, "/releases/v1.0.0/catalog.json.gz")
	if triggered() {
		t.Fatal("a negatively cached tag triggered a fetch")
	}
	get(t, s, "/releases/v1.0.2/catalog.json.gz")
	if !triggered() {
		t.Fatal("a new miss after 10s did not trigger a fetch")
	}
}

func TestSync_UpstreamDownKeepsServing(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publishBoth("v0.1.1", data1, cat1)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())
	before := get(t, s, "/index.yaml").Body.String()

	rel.srv.Close()
	if err := os.Rename(src.dir, src.dir+".gone"); err != nil {
		t.Fatal(err)
	}
	s.sync(context.Background())

	if n := testutil.ToFloat64(s.metrics.fetchFailures); n != 1 {
		t.Fatalf("fetch failures %v, want 1", n)
	}
	if after := get(t, s, "/index.yaml").Body.String(); after != before {
		t.Fatalf("index moved while upstream was down:\n%s", after)
	}
	if rec := get(t, s, "/releases/v0.1.1/catalog.json.gz"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cat1) {
		t.Fatalf("held latest: %d", rec.Code)
	}
	// Not built yet, but its sums were fetched: it builds from the mirror.
	if rec := get(t, s, "/releases/v0.1.0/catalog.json.gz"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cat0) {
		t.Fatalf("older tag: %d", rec.Code)
	}
	if rec := get(t, s, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("readyz %d", rec.Code)
	}
}

func TestMirror_FetchesOnlyTagsAndMain(t *testing.T) {
	src := newSourceRepo(t)
	main := src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", true)
	src.git(newCommit, "checkout", "--quiet", "-b", "fork")
	fork := src.commit(catalogTree(fixtureAPIVersion, "Unreviewed"), newCommit)
	src.git(time.Time{}, "update-ref", "refs/pull/1/head", fork)
	src.git(time.Time{}, "checkout", "--quiet", "main")

	m := &Mirror{Repo: Repo{Dir: t.TempDir() + "/mirror"}, URL: src.dir}
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	refs, err := m.git(ctx, gitCommandTime, "for-each-ref", "--format=%(refname)")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(refs)); strings.Join(got, " ") != "refs/heads/main refs/tags/v0.1.0" {
		t.Fatalf("mirror refs %v", got)
	}
	if _, err := m.git(ctx, gitCommandTime, "cat-file", "-e", fork+"^{commit}"); err == nil {
		t.Fatal("the pull-request commit reached the mirror")
	}
	tags, err := m.Tags(ctx)
	if err != nil || len(tags) != 1 || tags[0].Commit != main || !tags[0].Committed.Equal(oldCommit) {
		t.Fatalf("tags %+v, %v", tags, err)
	}
}

func TestServe_ConcurrentColdRequestsShareOneBuild(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publishBoth("v0.1.1", data1, cat1)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())
	before := testutil.ToFloat64(s.metrics.builds.WithLabelValues("catalog"))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if rec := get(t, s, "/releases/v0.1.0/catalog.json.gz"); rec.Code != http.StatusOK {
				t.Errorf("cold request: %d", rec.Code)
			}
		})
	}
	wg.Wait()
	if n := testutil.ToFloat64(s.metrics.builds.WithLabelValues("catalog")) - before; n != 1 {
		t.Fatalf("%v builds for one cold tag, want 1", n)
	}
}

func TestCache_EvictsUnpinnedOnly(t *testing.T) {
	c := newFileCache(10)
	latest := fileKey{"v0.1.2", assetCatalog}
	c.setPinned([]fileKey{latest})
	c.put(latest, make([]byte, 6))
	c.put(fileKey{"v0.1.0", assetCatalog}, make([]byte, 3))
	c.put(fileKey{"v0.1.1", assetCatalog}, make([]byte, 3))
	if _, ok := c.get(fileKey{"v0.1.0", assetCatalog}); ok {
		t.Fatal("least recently used file kept over budget")
	}
	if _, ok := c.get(latest); !ok {
		t.Fatal("pinned file evicted")
	}
	if c.bytes() != 9 {
		t.Fatalf("cache bytes %d, want 9", c.bytes())
	}
	c.put(fileKey{"v0.0.9", assetData}, make([]byte, 11))
	if _, ok := c.get(fileKey{"v0.0.9", assetData}); ok {
		t.Fatal("unpinned file larger than the budget cached")
	}
	c.setPinned(nil)
	c.put(fileKey{"v0.1.3", assetCatalog}, make([]byte, 5))
	if _, ok := c.get(latest); ok {
		t.Fatal("unpinned former latest kept over budget")
	}
}

func TestServe_PinnedLatestSurvivesEviction(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publishBoth("v0.1.1", data1, cat1)
	// Room for the pinned latest files only.
	s := newTestServer(t, src, rel, int64(len(data1)+len(cat1)))
	s.sync(context.Background())

	if rec := get(t, s, "/releases/v0.1.0/catalog.json.gz"); rec.Code != http.StatusOK {
		t.Fatalf("older tag: %d", rec.Code)
	}
	if _, ok := s.cache.get(fileKey{"v0.1.0", assetCatalog}); ok {
		t.Fatal("older tag cached beyond the budget")
	}
	for _, a := range assets {
		if _, ok := s.cache.get(fileKey{"v0.1.1", a}); !ok {
			t.Fatalf("pinned latest %s evicted", a.label())
		}
	}
}

func TestSumDue(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name               string
		committed, checked time.Time
		want               bool
	}{
		{"never checked", now.Add(-48 * time.Hour), time.Time{}, true},
		{"fresh tag, every sync", now.Add(-time.Hour), now.Add(-time.Minute), true},
		{"old tag, checked recently", now.Add(-48 * time.Hour), now.Add(-10 * time.Minute), false},
		{"old tag, an hour later", now.Add(-48 * time.Hour), now.Add(-time.Hour), true},
	}
	for _, c := range cases {
		if got := sumDue(c.committed, c.checked, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseSumFile(t *testing.T) {
	sum := sha256Hex([]byte("x"))
	for line, ok := range map[string]bool{
		sum + "  catalog.json.gz\n":                  true,
		sum + " *catalog.json.gz\n":                  true,
		sum + "\n":                                   true,
		strings.ToUpper(sum) + "  catalog.json.gz\n": true,
		sum + "  other.gz\n":                         false,
		sum[:63] + "  catalog.json.gz\n":             false,
		"":                                           false,
		sum + "  catalog.json.gz\n" + sum + "  x.gz\n": false,
	} {
		if _, err := parseSumFile([]byte(line), CatalogAsset); (err == nil) != ok {
			t.Errorf("%q: err %v, want ok=%v", line, err, ok)
		}
	}
}

func TestReadyz_BeforeFirstSync(t *testing.T) {
	src := newSourceRepo(t)
	s := newTestServer(t, src, newFakeReleases(t), 64<<20)
	if rec := get(t, s, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before sync: %d", rec.Code)
	}
	if rec := get(t, s, "/index.yaml"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("index before sync: %d", rec.Code)
	}
	if rec := get(t, s, "/livez"); rec.Code != http.StatusOK {
		t.Fatalf("livez: %d", rec.Code)
	}
}

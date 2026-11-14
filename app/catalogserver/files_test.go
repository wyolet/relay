package catalogserver

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestServe_RebuildMatchingPublishedIsServed(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", true)
	rel := newFakeReleases(t)
	data, cat := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data, cat)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.0"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("index (-want +got):\n%s", diff)
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
	for _, a := range assets {
		wantCount(t, a.label()+" mismatches", s.metrics.mismatches.WithLabelValues(a.label()), 0)
		wantCount(t, a.label()+" builds", s.metrics.builds.WithLabelValues(a.label()), 1)
	}
	wantAnswer(t, "readyz", answer{Code: http.StatusOK, Body: "ok\n"}, get(t, s, "/readyz"))
}

func TestServe_MismatchServesPublishedFile(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	data, _ := src.release("v0.1.0", "Rocket", oldCommit)
	// A catalog an older builder produced: valid, but not what this one builds.
	published := must(BuildCatalog(data, oldCommit.Add(time.Hour), "v0.1.0"))(t)
	rel.publishBoth("v0.1.0", data, published)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.0"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("index (-want +got):\n%s", diff)
	}
	rec := get(t, s, "/releases/v0.1.0/catalog.json.gz")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), published) {
		t.Fatalf("served %d, published bytes %v", rec.Code, bytes.Equal(rec.Body.Bytes(), published))
	}
	for label, want := range map[string]float64{"catalog": 1, "data": 0} {
		wantCount(t, label+" mismatches", s.metrics.mismatches.WithLabelValues(label), want)
	}
	if c, d := rel.fileHits("v0.1.0", CatalogAsset), rel.fileHits("v0.1.0", DataAsset("v0.1.0")); c != 1 || d != 0 {
		t.Fatalf("published files fetched: catalog %d, data %d; want 1, 0", c, d)
	}
}

func TestServe_BadPublishedFileIsNeverServedOrIndexed(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	data1, _ := src.release("v0.1.1", "Rocket 2", newCommit)
	rel.publish("v0.1.1", DataAsset("v0.1.1"), data1, data1)
	rel.publish("v0.1.1", CatalogAsset, gzipBytes(t, "tampered"), gzipBytes(t, "what CI built"))

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.0"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("index, where v0.1.1's catalog never verifies (-want +got):\n%s", diff)
	}
	want := answer{Code: http.StatusServiceUnavailable, Body: "file unavailable\n", Header: map[string]string{"Retry-After": "60", "Cache-Control": ""}}
	for range 2 {
		wantAnswer(t, "bad catalog", want, get(t, s, "/releases/v0.1.1/catalog.json.gz"))
	}
	// The failure is remembered: the sync and both requests share one download.
	if n := rel.fileHits("v0.1.1", CatalogAsset); n != 1 {
		t.Fatalf("bad catalog downloaded %d times, want 1", n)
	}
	if rec := get(t, s, "/releases/v0.1.1/"+DataAsset("v0.1.1")); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data1) {
		t.Fatalf("good data file of the same tag: %d", rec.Code)
	}
}

func TestServe_ConcurrentColdRequestsShareOneBuild(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	_, cat0 := rel.release(src, "v0.1.0", "Rocket", oldCommit)
	rel.release(src, "v0.1.1", "Rocket 2", newCommit)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())
	before := testutil.ToFloat64(s.metrics.builds.WithLabelValues("catalog"))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if rec := get(t, s, "/releases/v0.1.0/catalog.json.gz"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cat0) {
				t.Errorf("cold request: %d, body equal %v", rec.Code, bytes.Equal(rec.Body.Bytes(), cat0))
			}
		})
	}
	wg.Wait()
	if n := testutil.ToFloat64(s.metrics.builds.WithLabelValues("catalog")) - before; n != 1 {
		t.Fatalf("%v builds for one cold tag, want 1", n)
	}
}

func TestServe_PinnedLatestSurvivesEviction(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	data1, cat1 := src.release("v0.1.1", "Rocket 2", newCommit)
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
	for a, want := range map[asset][]byte{assetCatalog: cat1, assetData: data1} {
		if got, ok := s.cache.get(fileKey{"v0.1.1", a}); !ok || !bytes.Equal(got, want) {
			t.Fatalf("pinned latest %s: held %v, bytes equal %v", a.label(), ok, bytes.Equal(got, want))
		}
	}
}

package catalogserver

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestNew_RequiresEveryField(t *testing.T) {
	full := Config{RepoURL: "r", ReleasesURL: "u", MirrorDir: "m", SyncInterval: time.Second, CacheBytes: 1}
	for _, c := range []struct {
		want string
		edit func(*Config)
	}{
		{"catalogserver: repository URL is required", func(c *Config) { c.RepoURL = "" }},
		{"catalogserver: releases URL is required", func(c *Config) { c.ReleasesURL = "" }},
		{"catalogserver: mirror directory is required", func(c *Config) { c.MirrorDir = "" }},
		{"catalogserver: sync interval must be positive", func(c *Config) { c.SyncInterval = 0 }},
		{"catalogserver: cache budget must be positive", func(c *Config) { c.CacheBytes = 0 }},
	} {
		cfg := full
		c.edit(&cfg)
		if s, err := New(cfg); err == nil || err.Error() != c.want || s != nil {
			t.Errorf("err %v, want %q", err, c.want)
		}
	}
	s := must(New(full))(t)
	if got := s.hooks.userAgent; got != "wyolet-catalog-server/dev" {
		t.Fatalf("User-Agent without a version %q", got)
	}
}

func TestSumDue(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name               string
		committed, checked time.Time
		want               bool
	}{
		{"never checked", now.Add(-48 * time.Hour), time.Time{}, true},
		{"fresh tag, every sync", now.Add(-24*time.Hour + time.Second), now, true},
		{"a day old, checked just now", now.Add(-24 * time.Hour), now, false},
		{"old tag, checked under an hour ago", now.Add(-48 * time.Hour), now.Add(-time.Hour + time.Second), false},
		{"old tag, an hour later", now.Add(-48 * time.Hour), now.Add(-time.Hour), true},
	} {
		if got := sumDue(c.committed, c.checked, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIndex_WaitsForBothSums(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	data1, cat1 := src.release("v0.1.1", "Rocket 2", time.Now().Add(-time.Hour).Truncate(time.Second))
	rel.publish("v0.1.1", CatalogAsset, cat1, cat1)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())
	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.0"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("index before the data sum is published (-want +got):\n%s", diff)
	}
	want := answer{Code: http.StatusNotFound, Body: "404 page not found\n", Header: map[string]string{"Cache-Control": "no-store"}}
	wantAnswer(t, "unpublished data file", want, get(t, s, "/releases/v0.1.1/"+DataAsset("v0.1.1")))

	rel.publish("v0.1.1", DataAsset("v0.1.1"), data1, data1)
	s.sync(context.Background())
	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.1"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("index after both sums are published (-want +got):\n%s", diff)
	}
}

func TestIndex_UnknownChannelServesPublishedFiles(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	rel.release(src, "v0.1.0", "Rocket", oldCommit)
	commit := src.commit(catalogTree("relay.wyolet.dev/v1beta1", "Rocket"), newCommit)
	src.tag("v0.2.0", false)
	src.tag("v0.3.0-rc.1", false)
	data2 := must(BuildData(context.Background(), Repo{Dir: src.dir}, commit))(t)
	cat2 := gzipBytes(t, `{"version":"v0.2.0","schema":"a future one"}`)
	rel.publishBoth("v0.2.0", data2, cat2)
	rel.publishBoth("v0.3.0-rc.1", data2, cat2)

	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())

	if diff := cmp.Diff(map[string]string{"v1alpha2": "v0.1.0", "v1beta1": "v0.2.0"}, indexLatest(t, s)); diff != "" {
		t.Fatalf("index (-want +got):\n%s", diff)
	}
	rec := get(t, s, "/releases/v0.2.0/catalog.json.gz")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cat2) {
		t.Fatalf("published catalog of an unknown schema: %d", rec.Code)
	}
	wantCount(t, "catalog build failures", s.metrics.buildFailures.WithLabelValues("catalog"), 1)
	wantCount(t, "mismatches after a failed rebuild", s.metrics.mismatches.WithLabelValues("catalog"), 0)
	want := answer{Code: http.StatusNotFound, Body: "404 page not found\n", Header: map[string]string{"Cache-Control": ""}}
	wantAnswer(t, "pre-release tag", want, get(t, s, "/releases/v0.3.0-rc.1/catalog.json.gz"))
	wantIndex := "channels:\n    v1alpha2:\n        latest: v0.1.0\n    v1beta1:\n        latest: v0.2.0\n"
	if body := get(t, s, "/index.yaml").Body.String(); body != wantIndex {
		t.Fatalf("index.yaml:\n%s\nwant:\n%s", body, wantIndex)
	}
}

func TestSync_UpstreamDownKeepsServing(t *testing.T) {
	src := newSourceRepo(t)
	rel := newFakeReleases(t)
	_, cat0 := rel.release(src, "v0.1.0", "Rocket", oldCommit)
	_, cat1 := rel.release(src, "v0.1.1", "Rocket 2", newCommit)
	s := newTestServer(t, src, rel, 64<<20)
	s.sync(context.Background())
	const index = "channels:\n    v1alpha2:\n        latest: v0.1.1\n"
	if before := get(t, s, "/index.yaml").Body.String(); before != index {
		t.Fatalf("index:\n%s", before)
	}

	rel.srv.Close()
	if err := os.Rename(src.dir, src.dir+".gone"); err != nil {
		t.Fatal(err)
	}
	s.sync(context.Background())

	wantCount(t, "fetch failures", s.metrics.fetchFailures, 1)
	if after := get(t, s, "/index.yaml").Body.String(); after != index {
		t.Fatalf("index moved while upstream was down:\n%s", after)
	}
	// v0.1.0 was not built yet, but its sums were fetched: it builds from the mirror.
	for tag, want := range map[string][]byte{"v0.1.1": cat1, "v0.1.0": cat0} {
		if rec := get(t, s, "/releases/"+tag+"/catalog.json.gz"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Fatalf("%s catalog: %d, body equal %v", tag, rec.Code, bytes.Equal(rec.Body.Bytes(), want))
		}
	}
	wantAnswer(t, "readyz", answer{Code: http.StatusOK, Body: "ok\n"}, get(t, s, "/readyz"))
}

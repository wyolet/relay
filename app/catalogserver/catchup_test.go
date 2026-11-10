package catalogserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// runLoop runs s.Run until the test ends and waits for its first sync.
func runLoop(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "first sync", func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.synced
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two replicas sync on their own clocks. Once one indexes a new tag, a client
// that read that index must be able to fetch every file of the tag from the
// other one too, before that replica's next poll.
func TestReadiness_ReplicaBehindTheIndexServesItsFiles(t *testing.T) {
	for name, pushedBeforeLaggingSync := range map[string]bool{
		"tag known, sums not yet fetched": true,
		"tag not fetched yet":             false,
	} {
		t.Run(name, func(t *testing.T) {
			src := newSourceRepo(t)
			src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
			src.tag("v0.1.0", false)
			rel := newFakeReleases(t)
			data0, cat0 := src.releaseFiles("v0.1.0")
			rel.publishBoth("v0.1.0", data0, cat0)
			ahead := newTestServer(t, src, rel, 64<<20)
			lagging := newTestServer(t, src, rel, 64<<20)
			lagging.syncReqs.gap = 100 * time.Millisecond
			ctx := context.Background()
			ahead.sync(ctx)

			push := func() {
				src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), time.Now().Add(-time.Minute).Truncate(time.Second))
				src.tag("v0.1.1", false)
			}
			if pushedBeforeLaggingSync {
				push() // the lagging replica sees the tag while CI is still publishing
			}
			runLoop(t, lagging)
			if !pushedBeforeLaggingSync {
				push()
			}
			data1, cat1 := src.releaseFiles("v0.1.1")
			rel.publishBoth("v0.1.1", data1, cat1)
			ahead.sync(ctx)
			if got := indexLatest(t, ahead)[testChannel]; got != "v0.1.1" {
				t.Fatalf("ahead replica's index names %q", got)
			}

			for _, name := range []string{DataAsset("v0.1.1") + ".sha256", DataAsset("v0.1.1"), CatalogAsset + ".sha256", CatalogAsset} {
				if rec := get(t, lagging, "/releases/v0.1.1/"+name); rec.Code != http.StatusOK {
					t.Errorf("lagging replica: %s answered %d", name, rec.Code)
				}
			}
			rec := get(t, lagging, "/releases/v9.9.9/catalog.json.gz")
			if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unknown tag: %d %q, want an uncached 404", rec.Code, rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestGitHubHook_Debounced(t *testing.T) {
	s, _ := newHookServer(t, false)
	s.syncReqs.gap = 200 * time.Millisecond
	signalled := func() bool {
		select {
		case <-s.fetchNow:
			return true
		default:
			return false
		}
	}
	hook := func() int {
		req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(`{"ref":"refs/tags/v9.9.9","forged":true}`))
		req.Header.Set("X-GitHub-Event", "push")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for range 3 {
		if code := hook(); code != http.StatusAccepted {
			t.Fatalf("hook: %d, want 202", code)
		}
	}
	if !signalled() {
		t.Fatal("the first hook did not ask for a sync")
	}
	if signalled() {
		t.Fatal("hooks inside the gap asked for a second sync at once")
	}
	time.Sleep(300 * time.Millisecond)
	if !signalled() {
		t.Fatal("hooks inside the gap were dropped instead of synced at its end")
	}
	time.Sleep(300 * time.Millisecond)
	if signalled() {
		t.Fatal("a sync was asked for with no hook pending")
	}
	if n := testutil.ToFloat64(s.metrics.githubHooks); n != 3 {
		t.Fatalf("github hooks %v, want 3", n)
	}
}

func TestGitHubHook_FetchesWithoutWaitingForThePoll(t *testing.T) {
	src := newSourceRepo(t)
	src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", false)
	rel := newFakeReleases(t)
	data0, cat0 := src.releaseFiles("v0.1.0")
	rel.publishBoth("v0.1.0", data0, cat0)
	s := newTestServer(t, src, rel, 64<<20)
	runLoop(t, s)

	src.commit(catalogTree(fixtureAPIVersion, "Rocket 2"), newCommit)
	src.tag("v0.1.1", false)
	data1, cat1 := src.releaseFiles("v0.1.1")
	rel.publishBoth("v0.1.1", data1, cat1)
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("hook: %d", rec.Code)
	}
	waitFor(t, "v0.1.1 in the index", func() bool { return indexLatest(t, s)[testChannel] == "v0.1.1" })
}

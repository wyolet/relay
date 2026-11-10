package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/pkg/metrics"
)

// indexServer is a channel index answering latest(n) on its nth read.
type indexServer struct {
	reads atomic.Int32
	url   string
}

func newIndexServer(t *testing.T, latest func(read int32) string) *indexServer {
	t.Helper()
	s := &indexServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.reads.Add(1)
		fmt.Fprintf(w, "channels:\n    %s:\n        latest: %s\n", seed.Channel(), latest(n))
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// runRecheckFollower runs a follower on s whose seed lock is taken but seeds
// nothing, so its checks succeed without a database. The returned func stops
// Run and waits for it.
func runRecheckFollower(t *testing.T, s *indexServer, delay time.Duration) (*Follower, func()) {
	t.Helper()
	f := NewFollower(nil, BootstrapOptions{
		CatalogIndexURL: s.url,
		SeedTryLock: func(context.Context, func(context.Context) error) (bool, error) {
			return true, nil
		},
	})
	f.recheckDelay = delay
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx, 0); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return f, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hintUnseenChecks() float64 {
	return testutil.ToFloat64(metrics.CatalogChecks.WithLabelValues(metrics.CatalogCheckHintUnseen))
}

// A webhook announcing a release the index doesn't list yet gets exactly one
// more check, which finds it.
func TestFollowerRechecksWhenTheIndexLagsAWebhook(t *testing.T) {
	const delay = 50 * time.Millisecond
	s := newIndexServer(t, func(n int32) string {
		if n == 1 {
			return "v1.0.0"
		}
		return "v1.1.0"
	})
	unseen := hintUnseenChecks()
	f, _ := runRecheckFollower(t, s, delay)

	f.Nudge("v1.1.0")
	waitFor(t, "the re-check", func() bool { return s.reads.Load() >= 2 })
	time.Sleep(4 * delay)
	if n := s.reads.Load(); n != 2 {
		t.Fatalf("index reads = %d, want 2 (the check and one re-check)", n)
	}
	if got := hintUnseenChecks(); got != unseen {
		t.Fatalf("hint_unseen checks rose by %v, want 0", got-unseen)
	}
}

// A webhook naming a release the index already lists, an older one, or no
// semver tag at all starts no re-check.
func TestFollowerSkipsTheRecheckForAListedRelease(t *testing.T) {
	const delay = 50 * time.Millisecond
	s := newIndexServer(t, func(int32) string { return "v1.1.0" })
	f, _ := runRecheckFollower(t, s, delay)

	for i, tag := range []string{"v1.1.0", "v1.0.0", "my-fork-2", ""} {
		f.Nudge(tag)
		waitFor(t, "the check for "+tag, func() bool { return s.reads.Load() >= int32(i+1) })
	}
	time.Sleep(4 * delay)
	if n := s.reads.Load(); n != 4 {
		t.Fatalf("index reads = %d, want 4 (one per nudge, no re-check)", n)
	}
}

// When the re-check still doesn't find the release, the follower gives up,
// counts it as hint_unseen, and leaves the release to the poll.
func TestFollowerGivesUpAfterOneRecheck(t *testing.T) {
	const delay = 50 * time.Millisecond
	s := newIndexServer(t, func(int32) string { return "v1.0.0" })
	unseen := hintUnseenChecks()
	f, _ := runRecheckFollower(t, s, delay)

	f.Nudge("v1.1.0")
	waitFor(t, "the re-check", func() bool { return hintUnseenChecks() >= unseen+1 })
	time.Sleep(4 * delay)
	if n := s.reads.Load(); n != 2 {
		t.Fatalf("index reads = %d, want 2", n)
	}
	if got := hintUnseenChecks(); got != unseen+1 {
		t.Fatalf("hint_unseen checks rose by %v, want 1", got-unseen)
	}
}

// A second nudge while a re-check is pending checks at once but adds no
// timer; the one re-check looks for the newer release.
func TestFollowerFoldsNudgesIntoOneRecheck(t *testing.T) {
	const delay = 500 * time.Millisecond
	s := newIndexServer(t, func(n int32) string {
		if n <= 2 {
			return "v1.0.0"
		}
		return "v1.1.0"
	})
	unseen := hintUnseenChecks()
	f, _ := runRecheckFollower(t, s, delay)

	f.Nudge("v1.1.0")
	waitFor(t, "the first check", func() bool { return s.reads.Load() >= 1 })
	f.Nudge("v1.2.0")
	waitFor(t, "the re-check", func() bool { return hintUnseenChecks() >= unseen+1 })
	time.Sleep(2 * delay)
	if n := s.reads.Load(); n != 3 {
		t.Fatalf("index reads = %d, want 3 (two nudges, one re-check)", n)
	}
	if got := hintUnseenChecks(); got != unseen+1 {
		t.Fatalf("hint_unseen checks rose by %v, want 1", got-unseen)
	}
}

// Stopping Run cancels a pending re-check, and a pending re-check doesn't
// hold the follower: Check still runs meanwhile.
func TestFollowerRecheckStopsWithRun(t *testing.T) {
	const delay = 100 * time.Millisecond
	s := newIndexServer(t, func(int32) string { return "v1.0.0" })
	f, stop := runRecheckFollower(t, s, delay)

	f.Nudge("v1.1.0")
	waitFor(t, "the first check", func() bool { return s.reads.Load() >= 1 })
	if _, err := f.Check(context.Background()); err != nil {
		t.Fatalf("Check while a re-check is pending: %v", err)
	}
	stop()
	time.Sleep(3 * delay)
	if n := s.reads.Load(); n != 2 {
		t.Fatalf("index reads = %d, want 2 (no re-check after Run stopped)", n)
	}
}

func TestNewerTag(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", "v0.2.3", "v0.2.3"},
		{"v0.2.3", "", "v0.2.3"},
		{"v0.2.3", "v0.2.10", "v0.2.10"},
		{"v0.2.10", "v0.2.3", "v0.2.10"},
		{"my-fork", "v0.2.3", "v0.2.3"},
		{"v0.2.3", "my-fork", "v0.2.3"},
		{"my-fork", "", ""},
	} {
		if got := newerTag(tc.a, tc.b); got != tc.want {
			t.Errorf("newerTag(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

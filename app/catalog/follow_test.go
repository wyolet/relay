package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/metrics"
)

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v0.2.1", "v0.2.2", -1, true},
		{"v0.2.10", "v0.2.9", 1, true},
		{"v0.2.1", "v0.2.1", 0, true},
		{"v0.1.20", "v0.2.0", -1, true},
		{"v1.0.0-test", "v2.0.0-test", -1, true},
		{"my-fork-2", "v0.2.1", 0, false},
		{"v0.2.1", "", 0, false},
	} {
		cmp, ok := compareVersions(tc.a, tc.b)
		if cmp != tc.cmp || ok != tc.ok {
			t.Errorf("compareVersions(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, cmp, ok, tc.cmp, tc.ok)
		}
	}
}

// Nudges never wait for a check; those arriving while one is pending fold
// into it.
func TestFollowerNudgeNeverBlocks(t *testing.T) {
	f := NewFollower(nil, BootstrapOptions{})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			f.Nudge()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Nudge blocked with no check running")
	}
}

// With the timer off, Run checks only when nudged. The index fails here, and
// the failed check's status is written only under the seed lock, which
// another pod holds, so no store is touched.
func TestFollowerRunChecksOnNudge(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	f := NewFollower(nil, BootstrapOptions{
		CatalogIndexURL: srv.URL,
		SeedTryLock: func(context.Context, func(context.Context) error) (bool, error) {
			return false, nil
		},
	})
	failed := testutil.ToFloat64(metrics.CatalogChecks.WithLabelValues(metrics.CatalogCheckFailed))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx, 0); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(50 * time.Millisecond)
	if n := reads.Load(); n != 0 {
		t.Fatalf("index reads before a nudge = %d, want 0", n)
	}
	f.Nudge()
	deadline := time.Now().Add(5 * time.Second)
	for reads.Load() < 1 || testutil.ToFloat64(metrics.CatalogChecks.WithLabelValues(metrics.CatalogCheckFailed)) < failed+1 {
		if time.Now().After(deadline) {
			t.Fatal("the nudge started no check")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFollowerCheckReportsAnUnreadableIndex(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	f := NewFollower(nil, BootstrapOptions{
		CatalogIndexURL: srv.URL,
		SeedTryLock: func(context.Context, func(context.Context) error) (bool, error) {
			return false, nil
		},
	})
	_, err := f.Check(context.Background())
	if err == nil || errors.Is(err, ErrSeedBusy) {
		t.Fatalf("err = %v, want the index failure", err)
	}
}

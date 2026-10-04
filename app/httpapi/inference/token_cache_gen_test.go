package inference

import (
	"runtime"
	"testing"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// A cache entry lives until its token expires, so remembering the snapshot
// it resolved against by pointer keeps that whole catalog reachable — one
// retained copy per generation a token outlives.
// TestCacheDoesNotPinTheSnapshotItResolvedAgainst pins that only the
// generation number is kept.
func TestCacheDoesNotPinTheSnapshotItResolvedAgainst(t *testing.T) {
	ent := &cacheEntry{}
	collected := make(chan struct{})

	func() {
		snap := appcatalog.Build(nil, nil, nil, nil, nil, nil, nil, nil, nil)
		runtime.SetFinalizer(snap, func(*appcatalog.Snapshot) { close(collected) })
		ent.setSubjects(snap, []string{"user:u-1"})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-collected:
			// The entry still knows which view it was built from.
			next := appcatalog.Build(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			if _, ok := ent.subjectsFor(next); ok {
				t.Fatal("subjects survived into a different snapshot generation")
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("the snapshot was never collected — a cache entry still references it")
}

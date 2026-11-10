package catalog

// follow.go keeps a relay on RELAY_CATALOG_VERSION=latest on its channel's
// newest release while it runs. A check reads the channel index and seeds a
// newer release through seedRelease, the boot seed's path; the rows reach
// every pod's snapshot through the usual NOTIFY. Exact pins and "off" never
// run a follower: they reconcile at boot only.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/pkg/metrics"
)

// ErrSeedBusy reports a check skipped because another relay holds the seed lock.
var ErrSeedBusy = errors.New("catalog: another relay is seeding the catalog")

// CheckResult is the catalog-source version before and after one check.
type CheckResult struct {
	Previous string
	Current  string
	Changed  bool
}

// Follower re-checks the channel index on a timer, on Nudge, and on Check.
type Follower struct {
	stores *Stores
	opts   BootstrapOptions
	nudges chan struct{}

	// mu runs one check at a time on this pod and guards the index validator.
	mu   sync.Mutex
	etag string
}

// NewFollower checks against opts' index and archive URLs and seeds under
// opts.SeedTryLock. opts.AllowDowngrade is ignored: a follower never seeds an
// older release.
func NewFollower(stores *Stores, opts BootstrapOptions) *Follower {
	opts.AllowDowngrade = false
	return &Follower{stores: stores, opts: opts, nudges: make(chan struct{}, 1)}
}

// Nudge asks Run for a check without waiting for it; a nudge while one is
// already pending is dropped.
func (f *Follower) Nudge() {
	select {
	case f.nudges <- struct{}{}:
	default:
	}
}

// Run checks every interval (0 disables the timer) and on each Nudge until
// ctx ends.
func (f *Follower) Run(ctx context.Context, interval time.Duration) {
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-f.nudges:
		}
		if _, err := f.Check(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrSeedBusy) {
			slog.Error("catalog: check failed; keeping the seeded catalog", "err", err)
		}
	}
}

// Check reads the channel index and seeds its latest release when that is
// newer than the seeded one. A failure keeps the seeded catalog and is
// recorded in the catalog-source section; the index validator is kept only
// after a check that settled, so a failed release is retried in full.
func (f *Follower) Check(ctx context.Context) (CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx, err := seed.FetchLatest(ctx, f.opts.CatalogIndexURL, seed.Channel(), f.etag)
	if err != nil {
		f.etag = ""
		err = fmt.Errorf("read catalog index: %w", err)
		f.recordFailure(ctx, err)
		metrics.CatalogChecked(metrics.CatalogCheckFailed)
		return CheckResult{}, err
	}

	var res CheckResult
	outcome := seedUnchanged
	var seedErr error
	ran, err := f.tryLock(ctx, func(ctx context.Context) error {
		if idx.NotModified {
			cur, err := readCatalogSource(ctx, f.stores)
			if err != nil {
				return err
			}
			res.Previous, res.Current = cur.Version, cur.Version
		} else {
			st, err := readSeedState(ctx, f.stores)
			if err != nil {
				return err
			}
			res.Previous, res.Current = st.version(), st.version()
			outcome, seedErr = seedRelease(ctx, f.stores, f.opts, idx.Latest, st, false)
			if outcome == seedUpdated {
				res.Current = idx.Latest
			}
		}
		return writeCheckStatus(ctx, f.stores, time.Now(), seedErr)
	})
	res.Changed = res.Current != res.Previous
	switch {
	case err != nil:
		f.etag = ""
		metrics.CatalogChecked(metrics.CatalogCheckFailed)
		return res, err
	case !ran:
		f.etag = ""
		metrics.CatalogChecked(metrics.CatalogCheckSkippedLocked)
		return res, ErrSeedBusy
	case seedErr != nil:
		f.etag = ""
		metrics.CatalogChecked(metrics.CatalogCheckFailed)
		return res, fmt.Errorf("seed catalog %s: %w", idx.Latest, seedErr)
	}
	f.etag = idx.ETag
	metrics.CatalogChecked(checkResultLabel(outcome))
	return res, nil
}

// recordFailure writes a failed check's status when no other pod is seeding;
// the seeding pod writes its own.
func (f *Follower) recordFailure(ctx context.Context, checkErr error) {
	_, err := f.tryLock(ctx, func(ctx context.Context) error {
		return writeCheckStatus(ctx, f.stores, time.Now(), checkErr)
	})
	if err != nil && ctx.Err() == nil {
		slog.Warn("catalog: recording the failed check", "err", err)
	}
}

func (f *Follower) tryLock(ctx context.Context, fn func(context.Context) error) (bool, error) {
	if f.opts.SeedTryLock == nil {
		return true, fn(ctx)
	}
	return f.opts.SeedTryLock(ctx, fn)
}

func checkResultLabel(o seedOutcome) string {
	switch o {
	case seedUpdated:
		return metrics.CatalogCheckUpdated
	case seedSkippedOlder:
		return metrics.CatalogCheckSkippedOlder
	default:
		return metrics.CatalogCheckUnchanged
	}
}

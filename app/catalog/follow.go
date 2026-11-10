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

	"golang.org/x/mod/semver"

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

// defaultRecheckDelay is how long Run waits to check again when a release
// webhook names a release the index doesn't list yet. Catalog server
// replicas each sync git every 60 s, so the index read may have reached one
// that hasn't seen the release.
const defaultRecheckDelay = 75 * time.Second

// Follower re-checks the channel index on a timer, on Nudge, and on Check.
type Follower struct {
	stores *Stores
	opts   BootstrapOptions
	nudges chan struct{}

	// hintMu guards hint, the newest release a nudge named since Run last
	// took it.
	hintMu sync.Mutex
	hint   string

	recheckDelay time.Duration

	// mu runs one check at a time on this pod and guards the index validator
	// and the channel latest it was answered with.
	mu     sync.Mutex
	etag   string
	latest string
}

// NewFollower checks against opts' index and archive URLs and seeds under
// opts.SeedTryLock. opts.AllowDowngrade is ignored: a follower never seeds an
// older release.
func NewFollower(stores *Stores, opts BootstrapOptions) *Follower {
	opts.AllowDowngrade = false
	return &Follower{stores: stores, opts: opts, nudges: make(chan struct{}, 1), recheckDelay: defaultRecheckDelay}
}

// Nudge asks Run for a check without waiting for it; a nudge while one is
// already pending folds into it. tag is the release the nudge announced. It
// never decides what is seeded: when the check's index doesn't list it yet,
// Run checks once more after a delay.
func (f *Follower) Nudge(tag string) {
	f.hintMu.Lock()
	f.hint = newerTag(f.hint, tag)
	f.hintMu.Unlock()
	select {
	case f.nudges <- struct{}{}:
	default:
	}
}

func (f *Follower) takeHint() string {
	f.hintMu.Lock()
	defer f.hintMu.Unlock()
	h := f.hint
	f.hint = ""
	return h
}

// Run checks every interval (0 disables the timer) and on each Nudge until
// ctx ends. A nudge whose release the check's index doesn't list yet gets one
// more check after recheckDelay; nudges meanwhile only update that release.
func (f *Follower) Run(ctx context.Context, interval time.Duration) {
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	var recheck *time.Timer
	var recheckC <-chan time.Time
	stopRecheck := func() {
		if recheck != nil {
			recheck.Stop()
			recheck, recheckC = nil, nil
		}
	}
	defer stopRecheck()
	hint := ""
	for {
		rechecking := false
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-f.nudges:
			hint = newerTag(hint, f.takeHint())
		case <-recheckC:
			recheck, recheckC = nil, nil
			rechecking = true
		}
		rep, err := f.check(ctx)
		unseen := err == nil && hintUnseen(hint, rep.latest, rep.Current)
		if rechecking && unseen {
			rep.result = metrics.CatalogCheckHintUnseen
			slog.Info("catalog: the index still doesn't list the release a webhook announced; leaving it to the poll",
				"tag", hint, "latest", rep.latest)
		}
		metrics.CatalogChecked(rep.result)
		if err != nil && ctx.Err() == nil && !errors.Is(err, ErrSeedBusy) {
			slog.Error("catalog: check failed; keeping the seeded catalog", "err", err)
		}
		switch {
		case unseen && !rechecking:
			if recheckC == nil {
				recheck = time.NewTimer(f.recheckDelay)
				recheckC = recheck.C
			}
		case err != nil && recheckC != nil:
			// The pending re-check still runs for hint.
		default:
			hint = ""
			stopRecheck()
		}
	}
}

// Check reads the channel index and seeds its latest release when that is
// newer than the seeded one. A failure keeps the seeded catalog and is
// recorded in the catalog-source section; the index validator is kept only
// after a check that settled, so a failed release is retried in full.
func (f *Follower) Check(ctx context.Context) (CheckResult, error) {
	rep, err := f.check(ctx)
	metrics.CatalogChecked(rep.result)
	return rep.CheckResult, err
}

// checkReport is one check's result, the channel latest its index read
// named, and its CatalogChecks label.
type checkReport struct {
	CheckResult
	latest string
	result string
}

func (f *Follower) check(ctx context.Context) (checkReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx, err := seed.FetchLatest(ctx, f.opts.CatalogIndexURL, seed.Channel(), f.etag)
	if err != nil {
		f.etag = ""
		err = fmt.Errorf("read catalog index: %w", err)
		f.recordFailure(ctx, err)
		return checkReport{result: metrics.CatalogCheckFailed}, err
	}
	// A 304 only answers a validator kept from a settled read, which set latest.
	if !idx.NotModified {
		f.latest = idx.Latest
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
	rep := checkReport{CheckResult: res, latest: f.latest}
	switch {
	case err != nil:
		f.etag = ""
		rep.result = metrics.CatalogCheckFailed
		return rep, err
	case !ran:
		f.etag = ""
		rep.result = metrics.CatalogCheckSkippedLocked
		return rep, ErrSeedBusy
	case seedErr != nil:
		f.etag = ""
		rep.result = metrics.CatalogCheckFailed
		return rep, fmt.Errorf("seed catalog %s: %w", idx.Latest, seedErr)
	}
	f.etag = idx.ETag
	rep.result = checkResultLabel(outcome)
	return rep, nil
}

// newerTag returns the newer of two release tags, ignoring one that is not a
// semver tag; "" when neither is.
func newerTag(a, b string) string {
	switch {
	case !semver.IsValid(b):
		if semver.IsValid(a) {
			return a
		}
		return ""
	case !semver.IsValid(a), semver.Compare(b, a) > 0:
		return b
	default:
		return a
	}
}

// hintUnseen reports whether hint is a semver tag newer than every semver tag
// in seen, and at least one of them is one.
func hintUnseen(hint string, seen ...string) bool {
	compared := false
	for _, v := range seen {
		cmp, ok := compareVersions(hint, v)
		if !ok {
			continue
		}
		if cmp <= 0 {
			return false
		}
		compared = true
	}
	return compared
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

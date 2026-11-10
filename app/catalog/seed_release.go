package catalog

// seed_release.go seeds one named catalog release: the step the boot seed and
// the running follower share. Resolving which release to seed, and what a
// failure means for the process, stays with each caller.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/mod/semver"

	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/settings"
)

// seedOutcome is what seeding one release did.
type seedOutcome int

const (
	// seedUnchanged: the marker already names the release.
	seedUnchanged seedOutcome = iota
	// seedUpdated: the release was seeded and the marker moved to it.
	seedUpdated
	// seedSkippedOlder: the marker names a newer release, which stays.
	seedSkippedOlder
)

// seedState is the stored side of a seed decision.
type seedState struct {
	marker *settings.CatalogSource
	empty  bool
}

func (s seedState) version() string {
	if s.marker == nil {
		return ""
	}
	return s.marker.Version
}

// fetchError is a release that could not be downloaded or verified, as
// opposed to one that failed to seed; boot falls back differently on each.
type fetchError struct{ err error }

func (e *fetchError) Error() string { return e.err.Error() }
func (e *fetchError) Unwrap() error { return e.err }

func readSeedState(ctx context.Context, stores *Stores) (seedState, error) {
	marker, err := readCatalogSource(ctx, stores)
	if err != nil {
		return seedState{}, err
	}
	empty, err := isCatalogEmpty(ctx, stores)
	if err != nil {
		return seedState{}, fmt.Errorf("check empty: %w", err)
	}
	return seedState{marker: marker, empty: empty}, nil
}

func readCatalogSource(ctx context.Context, stores *Stores) (*settings.CatalogSource, error) {
	row, err := stores.Settings.Get(ctx, settings.SectionCatalogSource)
	if err != nil {
		return nil, fmt.Errorf("read catalog-source marker: %w", err)
	}
	cur, _ := row.Value.(*settings.CatalogSource)
	if cur == nil {
		cur = &settings.CatalogSource{}
	}
	return cur, nil
}

// seedRelease brings the catalog to version unless it is non-empty and the
// marker names version, or a newer release while allowOlder is false. A tree in
// AutoSeedDir stamped with version seeds from disk; otherwise the release is
// fetched and its sha256 checked, a failure coming back as *fetchError.
func seedRelease(ctx context.Context, stores *Stores, opts BootstrapOptions, version string, st seedState, allowOlder bool) (seedOutcome, error) {
	cur := st.version()
	if cur == version && !st.empty {
		return seedUnchanged, nil
	}
	if !st.empty && cur != "" {
		switch cmp, ok := compareVersions(version, cur); {
		case !ok:
			slog.Info("catalog: version is not a semver tag; reseeding because it differs from the seeded one",
				"version", version, "seeded", cur)
		case cmp < 0 && !allowOlder:
			slog.Warn("catalog: pinned version older than the seeded one; keeping it",
				"version", version, "seeded", cur)
			return seedSkippedOlder, nil
		case cmp < 0:
			slog.Warn("catalog: seeding a version older than the seeded one (RELAY_CATALOG_ALLOW_DOWNGRADE)",
				"version", version, "seeded", cur)
		}
	}

	if v := seed.DirVersion(opts.AutoSeedDir); v != "" && v == version {
		if err := seedAndMark(ctx, stores, opts, opts.AutoSeedDir, version, cur, "local"); err != nil {
			return seedUnchanged, err
		}
		return seedUpdated, nil
	}

	tmp, err := os.MkdirTemp("", "relay-catalog-*")
	if err != nil {
		return seedUnchanged, fmt.Errorf("catalog fetch tmpdir: %w", err)
	}
	defer os.RemoveAll(tmp)
	dataDir, err := seed.FetchCatalog(ctx, opts.CatalogURL, version, tmp)
	if err != nil {
		return seedUnchanged, &fetchError{err: err}
	}
	if err := seedAndMark(ctx, stores, opts, dataDir, version, cur, "fetched"); err != nil {
		return seedUnchanged, err
	}
	return seedUpdated, nil
}

// compareVersions orders two release tags by semver (-1: a is older than b);
// ok is false when either is not a semver tag, e.g. a self-stamped fork.
func compareVersions(a, b string) (int, bool) {
	if !semver.IsValid(a) || !semver.IsValid(b) {
		return 0, false
	}
	return semver.Compare(a, b), true
}

// seedAndMark runs the seed from dataDir and records version in the
// catalog-source marker. Rows the release no longer ships are detached; a
// failed seed writes no marker, so the next attempt retries.
func seedAndMark(ctx context.Context, stores *Stores, opts BootstrapOptions, dataDir, version, prev, source string) error {
	res, err := seed.Run(ctx, seed.Options{
		Pool: opts.Pool, YAMLDir: dataDir, Secrets: stores.secrets(),
		CatalogKindsOnly: true, DetachDropped: true,
	})
	if err != nil {
		return fmt.Errorf("seed catalog %s: %w", version, err)
	}
	if err := writeCatalogSource(ctx, stores, version); err != nil {
		return err
	}
	slog.Info("catalog: seeded version",
		"version", version, "previous", prev, "source", source,
		"models", res.Models, "bindings", res.HostBindings, "pricings", res.Pricings,
		"skipped_dirty", res.Skipped, "detached", res.Detached, "reattached", res.Reattached)
	return nil
}

// writeCatalogSource records a completed seed of version. The last check's
// time is kept; its error is cleared, since this seed succeeded.
func writeCatalogSource(ctx context.Context, stores *Stores, version string) error {
	cur, err := readCatalogSource(ctx, stores)
	if err != nil {
		return err
	}
	next := *cur
	next.Version = version
	next.SeededAt = time.Now().UTC().Format(time.RFC3339)
	next.LastError = ""
	return upsertCatalogSource(ctx, stores, &next)
}

func upsertCatalogSource(ctx context.Context, stores *Stores, v *settings.CatalogSource) error {
	marker, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal catalog-source marker: %w", err)
	}
	if _, err := stores.Settings.Upsert(ctx, settings.SectionCatalogSource, marker); err != nil {
		return fmt.Errorf("write catalog-source marker: %w", err)
	}
	return nil
}

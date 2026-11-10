package catalog

import (
	"context"
	"time"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/metrics"
)

// writeCheckStatus records a check's time and error in the catalog-source
// section, keeping its version. Callers hold the seed lock, so a seed on
// another pod can't interleave and lose its version.
func writeCheckStatus(ctx context.Context, stores *Stores, at time.Time, checkErr error) error {
	cur, err := readCatalogSource(ctx, stores)
	if err != nil {
		return err
	}
	next := *cur
	next.CheckedAt = at.UTC().Format(time.RFC3339)
	next.LastError = ""
	if checkErr != nil {
		next.LastError = checkErr.Error()
	}
	return upsertCatalogSource(ctx, stores, &next)
}

// publishSeededVersion sets the seeded-version gauge from this pod's settings
// cache. It runs on the listener goroutine for every catalog-source change.
func (c *Catalog) publishSeededVersion() {
	v, _ := c.Setting(settings.SectionCatalogSource)
	if cs, ok := v.(*settings.CatalogSource); ok && cs != nil {
		metrics.SetCatalogSeededVersion(cs.Version)
	}
}

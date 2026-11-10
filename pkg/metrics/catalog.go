package metrics

import "github.com/prometheus/client_golang/prometheus"

// CatalogApplyErrors counts catalog NOTIFY events that failed to apply
// incrementally. Each failure falls back to a full reload, so a rising rate
// means the store is flaky and pods are paying for rebuilds.
var CatalogApplyErrors = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "catalog_apply_errors_total",
		Help:      "Catalog change events that failed to apply incrementally, by kind.",
	},
	[]string{"kind"},
)

// Results of one catalog check, the values of CatalogChecks' result label.
const (
	CatalogCheckUnchanged     = "unchanged"
	CatalogCheckUpdated       = "updated"
	CatalogCheckFailed        = "failed"
	CatalogCheckSkippedLocked = "skipped_locked"
	CatalogCheckSkippedOlder  = "skipped_older"
	// CatalogCheckHintUnseen is a re-check after a release webhook whose
	// index still didn't list the announced release.
	CatalogCheckHintUnseen = "hint_unseen"
)

// CatalogChecks counts the catalog index checks a relay following the latest
// catalog release makes while running, by result.
var CatalogChecks = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "catalog_checks_total",
		Help:      "Catalog index checks made while running, by result (unchanged, updated, failed, skipped_locked, skipped_older, hint_unseen).",
	},
	[]string{"result"},
)

// CatalogSeededVersion is an info gauge: 1 for the catalog release the
// database was last seeded from, as this pod last read it.
var CatalogSeededVersion = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "catalog_seeded_version",
		Help:      "1 for the catalog release the database was last seeded from (label version).",
	},
	[]string{"version"},
)

func init() {
	Register(CatalogApplyErrors, CatalogChecks, CatalogSeededVersion)
	for _, r := range []string{CatalogCheckUnchanged, CatalogCheckUpdated, CatalogCheckFailed, CatalogCheckSkippedLocked, CatalogCheckSkippedOlder, CatalogCheckHintUnseen} {
		CatalogChecks.WithLabelValues(r)
	}
}

// CatalogApplyFailed is the one-liner the catalog listener calls when an
// event fails to apply.
func CatalogApplyFailed(kind string) { CatalogApplyErrors.WithLabelValues(SafeLabel(kind)).Inc() }

// CatalogChecked records one catalog check's result.
func CatalogChecked(result string) { CatalogChecks.WithLabelValues(result).Inc() }

// SetCatalogSeededVersion makes version the only series of the info gauge; an
// empty version (never seeded from a release) leaves none.
func SetCatalogSeededVersion(version string) {
	CatalogSeededVersion.Reset()
	if version != "" {
		CatalogSeededVersion.WithLabelValues(SafeLabel(version)).Set(1)
	}
}

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

func init() { Register(CatalogApplyErrors) }

// CatalogApplyFailed is the one-liner the catalog listener calls when an
// event fails to apply.
func CatalogApplyFailed(kind string) { CatalogApplyErrors.WithLabelValues(SafeLabel(kind)).Inc() }

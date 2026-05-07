// Package metrics owns the Prometheus registry and the /metrics HTTP handler.
// Consumers declare their own metrics (typically in their package's metrics.go),
// register them via Register, and use prometheus.Counter/Gauge/Histogram normally.
//
// # Conventions
//
// Every metric must set Namespace to metrics.Namespace ("relay") and pick a
// Subsystem matching the package name (e.g. "eventlog", "auth", "usage").
//
// Naming:
//   - Counters end in _total  (prometheus convention; client_golang enforces this)
//   - Gauges describe a current value (e.g. queue_depth)
//   - Histograms end in _seconds or _bytes
//
// Labels: use sparingly. High-cardinality labels (e.g. per-request IDs) explode
// memory. Prefer separate counters over a label with O(N) values.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace is the shared Prometheus namespace for every Relay metric.
// Concrete metrics SHOULD set this as their Namespace and pick a Subsystem
// matching their package (e.g. "eventlog", "auth", "usage").
const Namespace = "relay"

var registry = prometheus.NewRegistry()

func init() {
	// Default Go runtime + process metrics.
	registry.MustRegister(prometheus.NewGoCollector())
	registry.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
}

// Register adds collectors to the shared registry. Panics on duplicate
// registration (consistent with prometheus.MustRegister).
func Register(collectors ...prometheus.Collector) {
	registry.MustRegister(collectors...)
}

// Handler returns the http.Handler serving /metrics in Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

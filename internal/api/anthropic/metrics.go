package anthropic

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

var (
	metricMessagesRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: "anthropic",
			Name:      "messages_total",
			Help:      "Total Anthropic messages requests, labeled by model and status class (2xx/4xx/5xx).",
		},
		[]string{"model", "status"},
	)

	metricMessagesDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: "anthropic",
			Name:      "messages_duration_seconds",
			Help:      "End-to-end Anthropic messages request latency (handler entry to handler exit).",
			Buckets: []float64{
				0.001, 0.005, 0.01, 0.05, 0.1,
				0.25, 0.5, 1, 2, 5, 10, 30, 60, 120,
			},
		},
		[]string{"model"},
	)

	metricMessagesOverhead = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: "anthropic",
			Name:      "messages_overhead_seconds",
			Help:      "Per-request Relay overhead = total handler time minus upstream HTTP call duration. Only observed when upstream was reached.",
			Buckets:   []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5},
		},
		[]string{"model"},
	)
)

func init() {
	metrics.Register(metricMessagesRequests, metricMessagesDuration, metricMessagesOverhead)
}

// statusClass returns a bounded status label for an HTTP status code.
func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

// safeLabel returns "unknown" if s is empty.
func safeLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

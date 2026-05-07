package openai

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

var (
	metricChatRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metrics.Namespace,
			Subsystem: "chat",
			Name:      "requests_total",
			Help:      "Total chat completion requests, labeled by model and status class (2xx/4xx/5xx).",
		},
		[]string{"model", "status"},
	)

	metricChatDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: "chat",
			Name:      "request_duration_seconds",
			Help:      "End-to-end chat request latency (handler entry to handler exit).",
			// Wide buckets to capture real LLM latencies from cached/error responses
			// through long generation runs. Matches relay_upstream_duration_seconds.
			Buckets: []float64{
				0.001, 0.005, 0.01, 0.05, 0.1,       // sub-second (cached / errors)
				0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, // typical LLM range
			},
		},
		[]string{"model"},
	)

	metricChatOverhead = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metrics.Namespace,
			Subsystem: "chat",
			Name:      "overhead_seconds",
			Help:      "Per-request Relay overhead = total handler time minus upstream HTTP call duration. Only observed when upstream was reached (UpstreamDuration > 0).",
			// Tight buckets for sub-second overhead: 100µs → 500ms.
			Buckets: []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5},
		},
		[]string{"model"},
	)
)

func init() {
	metrics.Register(metricChatRequests, metricChatDuration, metricChatOverhead)
}

// statusClass returns a bounded status label ("2xx", "4xx", "5xx", etc.)
// for an HTTP status code. Using a class avoids high-cardinality label explosion.
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

// safeLabel returns "unknown" if s is empty, preventing empty-string Prometheus labels.
func safeLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

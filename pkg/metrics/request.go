package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// RequestTotal counts requests by shape (openai/anthropic/…), model, and HTTP status class.
	RequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "request_total",
			Help:      "Total requests handled, labeled by shape, model, and status class (2xx/4xx/5xx).",
		},
		[]string{"shape", "model", "status"},
	)

	// RequestDuration records end-to-end handler latency per shape and model.
	// Wide buckets capture real LLM latencies from cached/error responses through
	// long generation runs. Matches relay_upstream_duration_seconds.
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "request_duration_seconds",
			Help:      "End-to-end request latency (handler entry to handler exit).",
			Buckets: []float64{
				0.001, 0.005, 0.01, 0.05, 0.1,
				0.25, 0.5, 1, 2, 5, 10, 30, 60, 120,
			},
		},
		[]string{"shape", "model"},
	)

	// RequestOverhead records Relay processing overhead (total − upstream) per shape and model.
	// Tight buckets for sub-second overhead: 100µs → 500ms.
	RequestOverhead = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "request_overhead_seconds",
			Help:      "Per-request Relay overhead = total handler time minus upstream HTTP call duration. Only observed when upstream was reached.",
			Buckets:   []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5},
		},
		[]string{"shape", "model"},
	)
)

func init() {
	Register(RequestTotal, RequestDuration, RequestOverhead)
}

// StatusClass returns a bounded status label ("2xx", "4xx", "5xx", etc.)
// for an HTTP status code. Using a class avoids high-cardinality label explosion.
func StatusClass(code int) string {
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

// SafeLabel returns "unknown" if s is empty, preventing empty-string Prometheus labels.
func SafeLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

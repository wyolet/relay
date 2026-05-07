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
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 12),
		},
		[]string{"model"},
	)
)

func init() {
	metrics.Register(metricChatRequests, metricChatDuration)
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

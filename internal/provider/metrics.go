package provider

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

// MetricUpstreamDuration tracks the time spent in the actual upstream HTTP call
// (request fire → response body close). Provider clients (openai, ollama) call
// .Observe at the end of every request.
//
// Labels:
//   - provider: "openai" | "ollama" (matches catalog.ProviderKind)
//   - status:   "2xx" | "4xx" | "5xx" | "error" (network failure → "error")
//
// Buckets: same as relay_chat_request_duration_seconds — wide range to capture
// real LLM latencies from sub-second cached/error responses through long generation.
var MetricUpstreamDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "upstream",
		Name:      "duration_seconds",
		Help:      "Time spent in upstream provider HTTP calls (request fire to response close).",
		// Wide buckets matching relay_chat_request_duration_seconds so the two
		// histograms remain comparable in Grafana panels.
		Buckets: []float64{
			0.001, 0.005, 0.01, 0.05, 0.1,       // sub-second (cached / errors)
			0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, // typical LLM range
		},
	},
	[]string{"provider", "status"},
)

func init() {
	metrics.Register(MetricUpstreamDuration)
}

// StatusClass returns "2xx"/"3xx"/"4xx"/"5xx"/"error" for an HTTP status code,
// or "error" if status is 0 (network failure).
func StatusClass(code int) string {
	switch {
	case code == 0:
		return "error"
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

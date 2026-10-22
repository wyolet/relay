package metrics

import "github.com/prometheus/client_golang/prometheus"

// UsageExtractFallbacks counts buffered responses whose usage summary could not be read from where the shape keeps it (the ends of a JSON body, the usage-bearing events of a stream), so the whole body was decoded instead. adapter is the upstream shape; a sustained rate names the upstreams that place usage elsewhere.
var UsageExtractFallbacks = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "usage_extract_fallback_total",
		Help:      "Buffered responses whose usage was not found where the shape keeps it, so the whole body was decoded, by adapter.",
	},
	[]string{"adapter"},
)

func init() { Register(UsageExtractFallbacks) }

// UsageExtractFallback records one response decoded in full.
func UsageExtractFallback(adapter string) {
	UsageExtractFallbacks.WithLabelValues(adapter).Inc()
}

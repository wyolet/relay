package metrics

import "github.com/prometheus/client_golang/prometheus"

// KVErrors counts kv calls on the inference path that failed, by call site and by what relay did instead. A store that stalls or drops connections shows up here before it shows up as refused traffic.
var KVErrors = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "kv_errors_total",
		Help:      "Failed kv calls on the inference path, by op and outcome (refused = request rejected, fallback = decided without the stored state, dropped = write abandoned).",
	},
	[]string{"op", "outcome"},
)

// KV error outcomes.
const (
	KVRefused  = "refused"
	KVFallback = "fallback"
	KVDropped  = "dropped"
)

func init() { Register(KVErrors) }

// KVError is the one-liner a kv consumer calls when a call fails.
func KVError(op, outcome string) { KVErrors.WithLabelValues(op, outcome).Inc() }

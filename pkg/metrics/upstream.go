package metrics

import "github.com/prometheus/client_golang/prometheus"

// UpstreamConnections answers "is Relay re-dialing the provider on every
// request" — the connection-churn fear. The stdlib's MaxIdleConnsPerHost
// default of 2 silently cost ~40ms of upstream p50 before the 2026-07-08
// campaign found it by profiling; a new:reused ratio approaching the
// request rate names that failure in one glance. `kind` is "new" (dialed)
// or "reused" (from the keep-alive pool).
var UpstreamConnections = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "upstream_connections_total",
		Help:      "Upstream connections obtained per attempt, by kind (new = dialed, reused = keep-alive pool). new ≈ request rate means connection churn.",
	},
	[]string{"kind"},
)

// Pre-resolved children: GotConn fires once per upstream attempt, on the
// request hot path.
var (
	upstreamConnNew    = UpstreamConnections.WithLabelValues("new")
	upstreamConnReused = UpstreamConnections.WithLabelValues("reused")
)

func init() { Register(UpstreamConnections) }

// UpstreamConn is the one-liner the upstream transport's httptrace hook
// calls when a connection is handed to an attempt.
func UpstreamConn(reused bool) {
	if reused {
		upstreamConnReused.Inc()
	} else {
		upstreamConnNew.Inc()
	}
}

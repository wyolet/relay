package hybrid

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

var (
	counterModes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metrics.Namespace,
		Subsystem: "limits",
		Name:      "hybrid_counters",
		Help:      "Counters the hybrid limiter holds on this pod, by mode: local (admitted from memory) or exact (reserved in the store per request).",
	}, []string{"mode"})
	syncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "limits",
		Name:      "hybrid_syncs_total",
		Help:      "Sync round trips of the hybrid limiter, by outcome: ok, or failed when the store did not answer every command.",
	}, []string{"outcome"})

	// Resolved once: every sync reports.
	modeLocal, modeExact = counterModes.WithLabelValues("local"), counterModes.WithLabelValues("exact")
	syncOK, syncFailed   = syncs.WithLabelValues("ok"), syncs.WithLabelValues("failed")
)

func init() { metrics.Register(counterModes, syncs) }

func reportModes(local, exact int) {
	modeLocal.Set(float64(local))
	modeExact.Set(float64(exact))
}

func syncOutcome(failed bool) {
	if failed {
		syncFailed.Inc()
		return
	}
	syncOK.Inc()
}

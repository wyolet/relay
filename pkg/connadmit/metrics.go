package connadmit

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

var conns = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: metrics.Namespace,
	Subsystem: "inference",
	Name:      "conns",
	Help:      "Inference listener connections, by state: active (holding a slot, not idle), idle (holding a slot in keep-alive), queued (waiting for a slot).",
}, []string{"state"})

var refused = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "inference",
	Name:      "conns_refused_total",
	Help:      "Inference connections refused for want of a slot, by reason: queue_full and queue_timeout were answered 503, no_answer were closed unanswered because too many refusals were in progress.",
}, []string{"reason"})

var idleClosed = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "inference",
	Name:      "conns_idle_closed_total",
	Help:      "Idle keep-alive inference connections closed to give their slot to a queued connection.",
})

var (
	connsActive = conns.WithLabelValues("active")
	connsQueued = conns.WithLabelValues("queued")
	connsIdle   = conns.WithLabelValues("idle")

	refusedQueueFull    = refused.WithLabelValues("queue_full")
	refusedQueueTimeout = refused.WithLabelValues("queue_timeout")
	refusedNoAnswer     = refused.WithLabelValues("no_answer")
)

func init() { metrics.Register(conns, refused, idleClosed) }

type gaugeValues struct{ active, queued, idle int }

// publish moves the gauges by this listener's change since its last call, so several listeners in one process add up. Called with mu held.
func (l *Listener) publish() {
	now := gaugeValues{active: l.held - l.idleLen, queued: l.qLen, idle: l.idleLen}
	if d := now.active - l.shown.active; d != 0 {
		connsActive.Add(float64(d))
	}
	if d := now.queued - l.shown.queued; d != 0 {
		connsQueued.Add(float64(d))
	}
	if d := now.idle - l.shown.idle; d != 0 {
		connsIdle.Add(float64(d))
	}
	l.shown = now
}

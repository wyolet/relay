package metrics

import "github.com/prometheus/client_golang/prometheus"

// Health signals: the two "am I about to have a problem" metrics from
// docs/metrics.md — silent data loss (Q3) and provider keys dying (Q4).
// Both are one-liner emitters the owning packages call at the relevant
// moment; nothing here reaches back into them.
var (
	// RecordsLost counts background records dropped because a bounded
	// emitter queue was full (usage rows, payload captures). The async
	// contract mandates this counter: a drop you can't see is a billing
	// or audit hole. `kind` is the emitter: "usage" | "payload".
	RecordsLost = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "records_lost_total",
			Help:      "Background records dropped due to a full emitter queue, by kind (usage/payload).",
		},
		[]string{"kind"},
	)

	// ProviderKeysDown counts the times a pooled provider key was placed
	// into cooldown by a failure — the leading indicator of heading
	// toward "no healthy keys in pool". `reason` is the failure class
	// (auth/rate_limit/server_error/network/local_rl).
	//
	// A counter, not a gauge: breaker state lives in shared kv, so a
	// per-pod gauge of "keys down right now" would be inconsistent across
	// the fleet. Trip counts sum cleanly. A faithful global gauge is
	// deferred (would need a kv scan — not lightweight).
	ProviderKeysDown = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "provider_keys_down_total",
			Help:      "Times a pooled provider key was put into cooldown by a failure, by reason.",
		},
		[]string{"reason"},
	)
)

func init() {
	Register(RecordsLost, ProviderKeysDown)
}

// RecordLost is the one-liner an emitter calls at its drop site.
func RecordLost(kind string) { RecordsLost.WithLabelValues(SafeLabel(kind)).Inc() }

// ProviderKeyDown is the one-liner keypool calls when a key transitions
// into cooldown.
func ProviderKeyDown(reason string) { ProviderKeysDown.WithLabelValues(SafeLabel(reason)).Inc() }

// RegisterQueueDepth exposes a bounded emitter's queue depth as
// relay_emit_queue_depth{kind=...} — the leading signal for the drops
// RecordsLost counts after the fact. Same kinds as records_lost_total.
// fn is sampled at scrape time (chan len is concurrency-safe). Call once
// per kind at boot, where the emitter is constructed.
func RegisterQueueDepth(kind string, fn func() float64) {
	Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace:   Namespace,
		Name:        "emit_queue_depth",
		Help:        "Events waiting in a bounded emitter queue, by kind (usage/payload).",
		ConstLabels: prometheus.Labels{"kind": SafeLabel(kind)},
	}, fn))
}

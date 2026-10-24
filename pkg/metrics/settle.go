package metrics

import "github.com/prometheus/client_golang/prometheus"

// SettleQueueFull counts settles that found the post-flight settle queue full and ran on a round trip of their own. Nothing is dropped; a rate means the queue's writers fall behind.
var SettleQueueFull = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: Namespace,
	Name:      "settle_queue_full_total",
	Help:      "Post-flight settles that found the settle queue full and ran on their own Valkey round trip instead of a shared one.",
})

// SettleRetries counts queued settles whose shared round trip failed in transport (store unavailable, deadline) and were resent on a round trip of their own; the commit guard makes a resent commit that already ran a no-op.
var SettleRetries = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: Namespace,
	Name:      "settle_retries_total",
	Help:      "Queued post-flight settles resent on their own Valkey round trip after their shared round trip failed in transport.",
})

func init() { Register(SettleQueueFull, SettleRetries) }

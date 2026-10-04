package metrics

import "github.com/prometheus/client_golang/prometheus"

// StreamEventsDropped counts streamed events relay skipped because a shape
// translator could not convert them. Bytes have usually reached the caller by
// then, so the frame is skipped rather than the stream failed; this counter
// keeps the skip visible. adapter is the shape whose translator failed,
// direction is to_canonical (upstream frame) or from_canonical (inbound frame).
var StreamEventsDropped = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "stream_events_dropped_total",
		Help:      "Streamed events skipped because a shape translator could not convert them, by adapter, direction and reason.",
	},
	[]string{"adapter", "direction", "reason"},
)

func init() { Register(StreamEventsDropped) }

// StreamEventDropped records one skipped stream event.
func StreamEventDropped(adapter, direction, reason string) {
	StreamEventsDropped.WithLabelValues(adapter, direction, reason).Inc()
}

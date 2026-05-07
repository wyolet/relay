package eventlog

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

var (
	metricWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "eventlog",
		Name:      "written_total",
		Help:      "Total number of events successfully written to the backend.",
	})
	metricDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "eventlog",
		Name:      "dropped_total",
		Help:      "Total number of events dropped (buffer full, marshal error, or write error).",
	})
)

func init() {
	metrics.Register(metricWritten, metricDropped)
}

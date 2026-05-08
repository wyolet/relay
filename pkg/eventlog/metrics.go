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
		Help:      "Total events dropped (intake-buffer full, flushCh full, marshal error, send error, or closed logger).",
	})
	metricBatchDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "eventlog",
		Name:      "batch_dropped_total",
		Help:      "Number of full batches dropped because the flusher channel was full (back-pressure from a slow backend).",
	})
	metricSendError = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "eventlog",
		Name:      "send_error_total",
		Help:      "Number of batch send failures at the backend (CH PrepareBatch/Append/Send error).",
	})
	metricFlushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "eventlog",
		Name:      "flush_duration_seconds",
		Help:      "Time spent in sink.writeBatch (network + serialize). Independent from intake.",
		Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	})
)

func init() {
	metrics.Register(metricWritten, metricDropped, metricBatchDropped, metricSendError, metricFlushDuration)
}

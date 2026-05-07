package pipeline

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

var (
	metricPostFlightCommitErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "pipeline",
		Name:      "post_flight_commit_errors_total",
		Help:      "Number of async limit.Commit calls that failed after response was sent.",
	})
	metricPostFlightDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "pipeline",
		Name:      "post_flight_duration_seconds",
		Help:      "Time spent in the async post-flight goroutine (Commit + RecordSuccess).",
		Buckets:   []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	})
)

func init() { metrics.Register(metricPostFlightCommitErrors, metricPostFlightDuration) }

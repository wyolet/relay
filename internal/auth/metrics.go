package auth

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

var (
	metricRejectedMissing = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "auth",
		Name:      "rejected_missing_total",
		Help:      "Total requests rejected because the Authorization header was absent.",
	})
	metricRejectedInvalid = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "auth",
		Name:      "rejected_invalid_total",
		Help:      "Total requests rejected because the bearer token was invalid.",
	})
)

func init() {
	metrics.Register(metricRejectedMissing, metricRejectedInvalid)
}

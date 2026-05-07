package usage

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wyolet/relay/pkg/metrics"
)

var (
	metricMetadataRejectedOversize = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "usage",
		Name:      "metadata_rejected_oversize_total",
		Help:      "Total X-Relay-Metadata headers rejected because a field exceeded size limits.",
	})
	metricMetadataRejectedBadCharset = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "usage",
		Name:      "metadata_rejected_bad_charset_total",
		Help:      "Total X-Relay-Metadata headers rejected because a key or value contained invalid characters.",
	})
	metricMetadataRejectedMalformed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "usage",
		Name:      "metadata_rejected_malformed_total",
		Help:      "Total X-Relay-Metadata headers rejected because a pair had no '=' separator.",
	})
	metricDroppedSpans = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "usage",
		Name:      "dropped_spans_total",
		Help:      "Total OTel spans dropped due to batch processor queue overflow.",
	})
	metricDroppedEvents = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "usage",
		Name:      "dropped_events_total",
		Help:      "Total usage events dropped due to eventlog errors.",
	})
)

func init() {
	metrics.Register(
		metricMetadataRejectedOversize,
		metricMetadataRejectedBadCharset,
		metricMetadataRejectedMalformed,
		metricDroppedSpans,
		metricDroppedEvents,
	)
}

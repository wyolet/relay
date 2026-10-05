package otlpreceiver

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

const (
	resultAccepted    = "accepted"
	resultDisabled    = "disabled"
	resultBadRequest  = "bad_request"
	resultTooLarge    = "too_large"
	resultUnsupported = "unsupported_media"

	outcomeRecorded = "recorded"
	outcomeIgnored  = "ignored"
	outcomeRejected = "rejected"
)

// exportsTotal counts authenticated export requests by how they ended. Requests refused by the credential middleware are counted there.
var exportsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "exports_total",
	Help:      "OTLP trace export requests received, by result.",
}, []string{"result"})

// spansTotal answers how much of what clients export becomes usage: spans that are not model calls are ignored by design, so a high ignored share is normal for clients that export whole traces.
var spansTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "spans_total",
	Help:      "Spans in accepted OTLP exports, by outcome: recorded as a usage event, ignored (not a model call), or rejected (model call without span identity).",
}, []string{"outcome"})

func init() { metrics.Register(exportsTotal, spansTotal) }

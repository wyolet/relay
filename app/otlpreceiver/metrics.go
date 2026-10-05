package otlpreceiver

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

const (
	resultAccepted       = "accepted"
	resultDisabled       = "disabled"
	resultBadRequest     = "bad_request"
	resultTooLarge       = "too_large"
	resultTooManyRecords = "too_many_records"
	resultUnsupported    = "unsupported_media"
	resultRateLimited    = "rate_limited"
	resultBackpressure   = "backpressure"

	outcomeRecorded  = "recorded"
	outcomeIgnored   = "ignored"
	outcomeRejected  = "rejected"
	outcomeDuplicate = "duplicate"

	opMark   = "mark"
	opUnmark = "unmark"
)

// exportsTotal counts authenticated export requests by how they ended. Requests refused by the credential middleware are counted there.
var exportsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "exports_total",
	Help:      "OTLP trace export requests received, by result: accepted, disabled, bad_request, too_large, too_many_records, unsupported_media, rate_limited, or backpressure (usage queue full; the client is asked to resend).",
}, []string{"result"})

// spansTotal answers how much of what clients export becomes usage: spans that are not model calls are ignored by design, so a high ignored share is normal for clients that export whole traces.
var spansTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "spans_total",
	Help:      "Spans in OTLP exports, by outcome: recorded as a usage event, duplicate (a model call already recorded), ignored (not a model call), or rejected (a model call without span identity or starting in the future).",
}, []string{"outcome"})

// markerErrors counts kv failures of the duplicate check. A failed mark records the calls anyway, so they may be recorded again on a resend; a failed unmark leaves calls marked that were never queued, so a resend skips them.
var markerErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "marker_errors_total",
	Help:      "Failed kv operations of the OTLP receiver's duplicate check, by op: mark (calls recorded without the check) or unmark (calls left marked that were not queued).",
}, []string{"op"})

func init() { metrics.Register(exportsTotal, spansTotal, markerErrors) }

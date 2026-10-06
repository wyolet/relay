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
	resultPolicyDisabled = "policy_disabled"

	outcomeRecorded  = "recorded"
	outcomeIgnored   = "ignored"
	outcomeRejected  = "rejected"
	outcomeDuplicate = "duplicate"

	contentStored    = "stored"
	contentDuplicate = "duplicate"
	contentDropped   = "dropped"
	contentPolicy    = "policy"
	contentExpired   = "expired"

	opMark   = "mark"
	opUnmark = "unmark"
)

// exportsTotal counts authenticated export requests by how they ended. Requests refused by the credential middleware are counted there.
var exportsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "exports_total",
	Help:      "OTLP export requests received, by signal (traces, logs) and result: accepted, disabled, bad_request, too_large, too_many_records, unsupported_media, policy_disabled (the credential's policy is switched off), rate_limited, or backpressure (the usage queue is half full or lacks room; the client is asked to resend).",
}, []string{"signal", "result"})

// spansTotal answers how much of what clients export becomes usage: spans that are not model calls are ignored by design, so a high ignored share is normal for clients that export whole traces.
var spansTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "spans_total",
	Help:      "Spans in OTLP trace exports, by outcome: recorded as a usage event, duplicate (a model call already recorded), ignored (not a model call), or rejected (a model call without span identity, starting in the future, or older than the usage retention).",
}, []string{"outcome"})

// logRecordsTotal is spansTotal for the logs signal, where every record that is not the model-call event is ignored.
var logRecordsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "log_records_total",
	Help:      "Log records in OTLP logs exports, by outcome: recorded as a usage event, duplicate (a model call already recorded, by this signal or as a span), ignored (not a model-call event), or rejected (a model call with neither span ids nor a response id, starting in the future, or older than the usage retention).",
}, []string{"outcome"})

// contentTotal counts reported calls that carried message content while content capture was on.
var contentTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "content_total",
	Help:      "Reported model calls carrying message content while content capture is on, by outcome: stored (queued for the payload store), duplicate (content already stored for the call), dropped (the payload queue was half full or lacked room), policy (the reporter's policy does not capture payloads), or expired (the call is older than the payload retention).",
}, []string{"outcome"})

// markerErrors counts kv failures of the duplicate check. A failed mark stores the calls anyway, so they may be stored again on a resend; a failed unmark leaves calls marked that were never queued, so a resend skips them.
var markerErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "otlp",
	Name:      "marker_errors_total",
	Help:      "Failed kv operations of the OTLP receiver's duplicate check, by op: mark (calls stored without the check) or unmark (calls left marked that were not queued).",
}, []string{"op"})

func init() { metrics.Register(exportsTotal, spansTotal, logRecordsTotal, contentTotal, markerErrors) }

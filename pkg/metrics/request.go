package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The request-flow metrics (Rate / Errors / Duration) plus the Relay-vs-
// upstream split. See design/metrics.md for the questions these answer.
//
// Labelled by `source` (the runner: pipeline/proxy/ws/batch) — the
// lowest-cardinality dimension already on the lifecycle Context. Wire
// `shape` is deliberately NOT a label: it isn't carried on the Context
// and plumbing it would touch the inference handler. Add it the day the
// question "openai vs anthropic traffic split" is actually asked.
var (
	// RequestsTotal answers "how much traffic, how many errors" (Q1/Q2).
	// `status` is a bounded class (2xx/4xx/5xx), never the raw code.
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "requests_total",
			Help:      "Total requests handled, by source runner and status class (2xx/4xx/5xx).",
		},
		[]string{"source", "status"},
	)

	// RequestSeconds is total end-to-end time (Q2 — the "whose time is
	// it" numerator). Wide buckets span cached/error responses through
	// long generation runs.
	RequestSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "request_seconds",
			Help:      "End-to-end request latency (handler entry to response closed).",
			Buckets: []float64{
				0.001, 0.005, 0.01, 0.05, 0.1,
				0.25, 0.5, 1, 2, 5, 10, 30, 60, 120,
			},
		},
		[]string{"source"},
	)

	// OverheadSeconds is THE wedge metric (Q1): Relay's own time, total
	// minus the upstream call. The performance contract puts a hard SLO
	// here (p99 < 10ms live). Tight sub-second buckets: 100µs → 500ms.
	OverheadSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "overhead_seconds",
			Help:      "Relay overhead = total handler time minus the upstream call. Observed only when upstream was reached.",
			Buckets:   []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5},
		},
		[]string{"source"},
	)

	// InflightRequests answers "how many streams are open right now /
	// where does concurrency plateau" (Q5). A streamed request counts
	// until its body closes (post-flight fires at Close).
	InflightRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "inflight_requests",
			Help:      "Currently-open requests, by source runner. Streams count until body close.",
		},
		[]string{"source"},
	)

	// AdmissionSeconds is Timing.Start → upstream handoff: auth +
	// rate-limit reserve + key selection — the Redis-contention proxy
	// (Q5). Skipped when the request never reached upstream. Same
	// sub-second bucket style as OverheadSeconds: 100µs → 250ms.
	AdmissionSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "admission_seconds",
			Help:      "Time from request accept to upstream handoff (auth + rate-limit reserve + key selection). Observed only when upstream was reached.",
			Buckets:   []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25},
		},
		[]string{"source"},
	)

	// PostFlightSeconds is the duration of one full post-flight fan-out
	// (Registry.Finalize): is post-flight keeping up with stream closes,
	// or building a goroutine backlog (Q5)?
	PostFlightSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "post_flight_seconds",
			Help:      "Duration of the post-flight observer fan-out (hooks + collectors) per request.",
			Buckets:   []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
	)
)

// Pre-resolved gauge children: the inflight inc runs on the request hot
// path, where a per-request WithLabelValues lookup is avoidable — the
// source set is fixed.
var (
	inflightPipeline = InflightRequests.WithLabelValues("pipeline")
	inflightProxy    = InflightRequests.WithLabelValues("proxy")
	inflightWS       = InflightRequests.WithLabelValues("ws")
	inflightBatch    = InflightRequests.WithLabelValues("batch")
	inflightUnknown  = InflightRequests.WithLabelValues("unknown")
)

func init() {
	Register(RequestsTotal, RequestSeconds, OverheadSeconds, InflightRequests, AdmissionSeconds, PostFlightSeconds)
}

func inflightGauge(source string) prometheus.Gauge {
	switch source {
	case "pipeline":
		return inflightPipeline
	case "proxy":
		return inflightProxy
	case "ws":
		return inflightWS
	case "batch":
		return inflightBatch
	default:
		return inflightUnknown
	}
}

// InflightInc marks a request admitted. Hot path: O(1), allocation-free.
func InflightInc(source string) { inflightGauge(source).Inc() }

// InflightDec marks a request finalized. Callers must pair with a prior
// InflightInc for the same request or the gauge goes negative.
func InflightDec(source string) { inflightGauge(source).Dec() }

// RecordAdmission is the one-liner the post-flight metrics observer calls
// when the request reached upstream (Timing.Upstream.Start was stamped).
func RecordAdmission(source string, admission time.Duration) {
	AdmissionSeconds.WithLabelValues(SafeLabel(source)).Observe(admission.Seconds())
}

// RecordPostFlight is the Finalize-duration observer wired onto
// lifecycle.Registry at boot (the Registry stays metrics-free).
func RecordPostFlight(d time.Duration) { PostFlightSeconds.Observe(d.Seconds()) }

// RecordServed is the one-liner the post-flight metrics observer calls
// once per finalized request. total is the full handler time; upstream
// is the upstream-call duration (zero when upstream wasn't reached, in
// which case the overhead split is not observed — it'd be meaningless).
func RecordServed(source string, status int, total, upstream time.Duration) {
	src := SafeLabel(source)
	RequestsTotal.WithLabelValues(src, StatusClass(status)).Inc()
	RequestSeconds.WithLabelValues(src).Observe(total.Seconds())
	if upstream > 0 {
		OverheadSeconds.WithLabelValues(src).Observe((total - upstream).Seconds())
	}
}

// StatusClass returns a bounded status label ("2xx", "4xx", "5xx", etc.)
// for an HTTP status code. Using a class avoids high-cardinality label explosion.
func StatusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

// SafeLabel returns "unknown" if s is empty, preventing empty-string Prometheus labels.
func SafeLabel(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

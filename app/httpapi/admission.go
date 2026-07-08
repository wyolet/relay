package httpapi

// Admission is the per-pod in-flight cap: a counting semaphore that bounds
// how many inference requests can be in flight at once, so a flood degrades
// into a flat ceiling of fast 429s instead of an unbounded goroutine/RSS
// spiral (the load test's 5krps knee hit 53k goroutines / 1.3GB before this).
//
// It rides the lifecycle spine, not a chi middleware, for two reasons:
//   - scope: PreFlight runs only for Dispatch-driven inference requests (and
//     each WS frame), never /healthz, /v1/models, or the control plane — so a
//     shed request is always a real inference request, never a health probe.
//   - lifetime: a streamed response holds its slot until the response body
//     closes, not until the handler returns. The release is a Collector, which
//     the runner fires from Finalize at Body.Close() — the same hook that
//     stamps Timing.End. Releasing on handler return would free the slot while
//     the stream is still open.
//
// A request that acquired a slot is marked in lc.Metadata; the Collector
// releases only when that mark is present, so a shed request (which never
// acquired) can't drive the semaphore negative, and Finalize's once-per-
// request guarantee means the release runs exactly once.

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// DefaultMaxInflight is the derived cap when RELAY_MAX_INFLIGHT is unset
// (or <= 0). Justification from the 2026-07-08 load test (12-core ARM pod):
// healthy load cleared ~1k rps at ~20ms → ~20 concurrent, and the achieved
// knee (~2,856 rps) sat far below this while still fast. So this cap never
// throttles healthy load — it only engages once latency degrades enough that
// concurrency climbs three orders of magnitude toward the observed 53k-
// goroutine / 1.3GB blowup, converting that spiral into a bounded ceiling.
// It is a fixed number, not 256×GOMAXPROCS: the binding constraint is per-pod
// memory (set by GOMEMLIMIT / the container limit), which does not scale with
// core count, so a per-core multiple is the wrong axis and runs loose on big
// pods.
const DefaultMaxInflight = 2048

// RetryAfterShed is the Retry-After header value (seconds) sent with a shed
// 429. One second is far longer than the ~20ms a healthy request path takes,
// so the transient saturation has cleared by the time a well-behaved client
// retries.
const RetryAfterShed = "1"

// ErrShed is returned by PreFlight when the in-flight cap is reached. The
// inference dispatch maps it to a retriable 429 + Retry-After; any other
// pre-flight error stays a 500.
var ErrShed = errors.New("in-flight capacity reached")

// admittedKey marks (in lc.Metadata, the documented pre-flight-write /
// post-flight-read channel) that THIS request holds a semaphore slot. The
// Collector's release is gated on it so a shed request never releases a slot
// it didn't take.
const admittedKey = "httpapi.admission.held"

// requestsShedTotal answers the operator's fear "am I turning customers away
// because I'm overloaded" (metrics fear-naming rule) — not "is the semaphore
// full". It is the one signal admission control adds.
var requestsShedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Name:      "requests_shed_total",
	Help:      "Inference requests refused at admission because the in-flight cap was reached (load shedding).",
})

func init() { metrics.Register(requestsShedTotal) }

// Admission bounds concurrent in-flight inference requests via a buffered
// channel used as a counting semaphore.
type Admission struct {
	sem chan struct{}
}

// NewAdmission returns an Admission capped at max concurrent in-flight
// requests. max <= 0 uses DefaultMaxInflight.
func NewAdmission(max int) *Admission {
	if max <= 0 {
		max = DefaultMaxInflight
	}
	return &Admission{sem: make(chan struct{}, max)}
}

// Cap reports the configured in-flight ceiling.
func (a *Admission) Cap() int { return cap(a.sem) }

// PreFlight is a lifecycle.PreFlightMiddleware: it acquires a slot without
// blocking. On success it marks lc so the Collector knows to release; when the
// cap is reached it counts the shed and returns ErrShed for the dispatch to
// map to a 429. A request whose Context can't carry the release mark is
// admitted rather than held (never take a slot we can't return).
func (a *Admission) PreFlight(_ context.Context, lc *lifecycle.Context, _ *lifecycle.PreFlightEvent) error {
	if lc == nil || lc.Metadata == nil {
		return nil
	}
	select {
	case a.sem <- struct{}{}:
		lc.Metadata[admittedKey] = struct{}{}
		return nil
	default:
		requestsShedTotal.Inc()
		return ErrShed
	}
}

// Collect is a lifecycle.Collector: it releases the slot this request holds.
// Runs on every Finalize (which fires at response-body close), so a streamed
// request holds its slot for the whole stream. Gated on the admit mark so a
// shed request is a no-op; the non-blocking receive is a belt-and-suspenders
// guard against ever draining below zero.
func (a *Admission) Collect(lc *lifecycle.Context) {
	if lc == nil {
		return
	}
	if _, ok := lc.Metadata[admittedKey]; !ok {
		return
	}
	select {
	case <-a.sem:
	default:
	}
}

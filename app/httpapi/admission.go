package httpapi

// Admission is the per-pod in-flight cap: a counting semaphore that bounds
// how many inference requests can be in flight at once, so a flood degrades
// into a flat ceiling of fast overload refusals instead of an unbounded goroutine/RSS
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
// Sheds happen at two stages. The inference routes check Full before reading
// the body and refuse there (early), so a flood costs no body read, parse or
// lifecycle work; Full takes no slot, so PreFlight stays the one place a slot
// is acquired, and a request that passed the early check can still be shed
// there when the last slot went in between (late).
//
// A request that acquired a slot is marked in lc.Metadata; the Collector
// releases only when that mark is present, so a shed request (which never
// acquired) can't drive the semaphore negative, and Finalize's once-per-
// request guarantee means the release runs exactly once.

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

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

// RetryAfterShed is the Retry-After header value (seconds) sent when relay
// refuses a request as overloaded. One second is far longer than the ~20ms a healthy request path takes,
// so the transient saturation has cleared by the time a well-behaved client
// retries.
const RetryAfterShed = "1"

// OverloadMessage is the error message of a request refused as overloaded.
const OverloadMessage = "relay is at capacity; retry shortly"

// ErrShed is returned by PreFlight when the in-flight cap is reached. The
// inference dispatch maps it to the route's retriable overload answer; any other
// pre-flight error stays a 500.
var ErrShed = errors.New("in-flight capacity reached")

// Shed stages, the values of requests_shed_total's stage label.
const (
	ShedStageEarly  = "early"   // refused at the route, before the body is read
	ShedStageLate   = "late"    // refused by PreFlight, after the body was read
	ShedStageKVPool = "kv_pool" // refused at the route, before the body is read, while the kv pool's wait queue is full
)

// admittedKey marks (in lc.Metadata, the documented pre-flight-write /
// post-flight-read channel) that THIS request holds a semaphore slot. The
// Collector's release is gated on it so a shed request never releases a slot
// it didn't take.
const admittedKey = "httpapi.admission.held"

// requestsShedTotal answers the operator's fear "am I turning customers away
// because I'm overloaded" (metrics fear-naming rule) — not "is the semaphore
// full". It is the one signal admission control adds.
var requestsShedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Name:      "requests_shed_total",
	Help:      "Inference requests refused at admission (load shedding), by stage: early (in-flight cap reached, before the body is read), late (in-flight cap reached, after) or kv_pool (the kv connection wait queue full, before the body is read).",
}, []string{"stage"})

var (
	shedEarlyTotal  = requestsShedTotal.WithLabelValues(ShedStageEarly)
	shedLateTotal   = requestsShedTotal.WithLabelValues(ShedStageLate)
	shedKVPoolTotal = requestsShedTotal.WithLabelValues(ShedStageKVPool)
)

func init() { metrics.Register(requestsShedTotal) }

// shedLogInterval spaces the shed WARN: a flood sheds thousands of requests a
// second and a line for each would cost more than the refusal itself.
const shedLogInterval = time.Second

var (
	shedEarlySinceLog  atomic.Int64
	shedLateSinceLog   atomic.Int64
	shedKVPoolSinceLog atomic.Int64
	shedLastLogNanos   atomic.Int64
)

// RecordShed counts one shed request at stage (a ShedStage* value) and logs
// at most one WARN per shedLogInterval process-wide, carrying the sheds since
// the previous line.
func RecordShed(stage string) {
	switch stage {
	case ShedStageEarly:
		shedEarlyTotal.Inc()
		shedEarlySinceLog.Add(1)
	case ShedStageKVPool:
		shedKVPoolTotal.Inc()
		shedKVPoolSinceLog.Add(1)
	default:
		shedLateTotal.Inc()
		shedLateSinceLog.Add(1)
	}
	now := time.Now().UnixNano()
	last := shedLastLogNanos.Load()
	if now-last < int64(shedLogInterval) || !shedLastLogNanos.CompareAndSwap(last, now) {
		return
	}
	early, late, kvPool := shedEarlySinceLog.Swap(0), shedLateSinceLog.Swap(0), shedKVPoolSinceLog.Swap(0)
	slog.Warn("inference: shedding load", "shed", early+late+kvPool, "early", early, "late", late, "kv_pool", kvPool)
}

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
	// Publish the cap so dashboards can plot inflight/limit headroom —
	// requests_shed_total fires when it's already too late; the ratio is
	// the leading signal.
	metrics.SetInflightLimit(max)
	return &Admission{sem: make(chan struct{}, max)}
}

// Cap reports the configured in-flight ceiling.
func (a *Admission) Cap() int { return cap(a.sem) }

// Full reports whether every slot is held. A lock-free snapshot that takes no
// slot: a request it lets through can still be shed by PreFlight. Nil-safe
// (never full).
func (a *Admission) Full() bool {
	return a != nil && len(a.sem) >= cap(a.sem)
}

// PreFlight is a lifecycle.PreFlightMiddleware: it acquires a slot without
// blocking. On success it marks lc so the Collector knows to release; when the
// cap is reached it counts the shed and returns ErrShed for the dispatch to
// map to the route's overload answer. A request whose Context can't carry the release mark is
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
		RecordShed(ShedStageLate)
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

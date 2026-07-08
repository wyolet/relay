// Package metricslog is the Prometheus observer on the lifecycle spine.
// It owns no metrics of its own — it reads the per-request lifecycle
// Context + outcome and forwards them to the one-liner emitters in
// pkg/metrics. Registering it is the entire wiring for the request-flow
// metrics (Q1–Q2, Q5); no runner (pipeline/proxy/ws/
// batch) changes, because every runner already feeds the lifecycle spine.
//
// It registers on four seams: pre-flight (inflight gauge up), the
// post-flight Hook (request-flow histograms for buffered requests), a
// stream observer (the same histograms for streamed requests, whose Fill is
// skipped once a stream session pre-fills the collected set), and a
// Collector (inflight gauge down — Collect runs on every Finalize, unlike
// Fill which a pre-send stream fill can mark done).
//
// Out of scope: the data-loss and provider-key-health metrics (Q3/Q4)
// are emitted at their sources (the emitters, keypool) — they don't pass
// through here.
package metricslog

import (
	"context"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// inflightKey marks (in lc.Metadata, the documented cross-hook channel)
// that THIS request incremented the inflight gauge. The decrement is
// gated on it so a runner that never ran pre-flight (batch today) can't
// drive the gauge negative.
const inflightKey = "metrics.inflight"

// Hook reads request outcome + timing and emits the request-flow metrics.
// It is a pure lifecycle.Hook: it produces no stored result (returns nil)
// and never mutates the Context post-flight. Emitting a Prometheus sample
// is a cheap, non-blocking atomic op, so it satisfies the "Fill must be
// cheap" rule.
type Hook struct{}

// New constructs the (stateless) metrics observer.
func New() *Hook { return &Hook{} }

func (*Hook) Name() string { return "metrics" }

// PreFlight marks the request admitted: the inflight gauge rises and the
// Metadata note arms the post-flight decrement. O(1), never errors.
func (*Hook) PreFlight(_ context.Context, lc *lifecycle.Context, _ *lifecycle.PreFlightEvent) error {
	if lc == nil || lc.Metadata == nil {
		return nil
	}
	metrics.InflightInc(lc.Source)
	lc.Metadata[inflightKey] = true
	return nil
}

func (*Hook) Fill(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
	metrics.RecordServed(lc.Source, ev.Status, lc.Timing.End, upstreamDuration(lc))
	if adm := lc.Timing.Upstream.Start; adm > 0 {
		metrics.RecordAdmission(lc.Source, adm)
	}
	return nil, nil
}

// Collect lowers the inflight gauge for a finalized request. It is a
// Collector, not part of Fill, because Fill is skipped when a stream
// session pre-filled the request — Collect runs on every Finalize.
func (*Hook) Collect(lc *lifecycle.Context) {
	if lc == nil {
		return
	}
	if _, ok := lc.Metadata[inflightKey]; ok {
		metrics.InflightDec(lc.Source)
	}
}

// NewObserver makes the metrics observer also a StreamObserverFactory. A
// streamed request pre-fills its collected set, which marks the Context
// filled and skips the post-flight Fill fan-out — so the request-flow
// histograms Fill emits would be lost for every stream. This observer
// re-emits them at end-of-stream instead. It stores nothing (returns nil),
// so it never collides with a real producer under the "metrics" name.
func (*Hook) NewObserver(lc *lifecycle.Context) lifecycle.StreamObserver {
	return &streamObserver{lc: lc}
}

// streamObserver emits the request-flow metrics for one streamed request at
// Result (end-of-stream), reading the outcome from the Context — the status
// the runner stamped and the timing marks. It ignores frames.
type streamObserver struct{ lc *lifecycle.Context }

func (*streamObserver) Observe([]byte) {}

func (o *streamObserver) Result() (any, error) {
	lc := o.lc
	metrics.RecordServed(lc.Source, lc.ResponseStatus, lc.Timing.End, upstreamDuration(lc))
	if adm := lc.Timing.Upstream.Start; adm > 0 {
		metrics.RecordAdmission(lc.Source, adm)
	}
	return nil, nil
}

// upstreamDuration is the upstream-call leg, or 0 when upstream wasn't
// reached (RecordServed then skips the overhead split — it'd be bogus).
func upstreamDuration(lc *lifecycle.Context) time.Duration {
	up := lc.Timing.Upstream
	if up.Start > 0 && up.ResponseEnd > up.Start {
		return up.ResponseEnd - up.Start
	}
	return 0
}

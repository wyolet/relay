// Package metricslog is the Prometheus observer on the lifecycle spine.
// It owns no metrics of its own — it reads the per-request lifecycle
// Context + outcome in the post-flight phase and forwards them to the
// one-liner emitters in pkg/metrics. Registering it is the entire wiring
// for the request-flow metrics (Q1–Q3 in docs/metrics.md); no runner
// (pipeline/proxy/ws/batch) changes, because every runner already feeds
// the lifecycle spine.
//
// Out of scope: the data-loss and provider-key-health metrics (Q3/Q4)
// are emitted at their sources (the emitters, keypool) — they don't pass
// through here.
package metricslog

import (
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// Hook reads request outcome + timing and emits the request-flow metrics.
// It is a pure lifecycle.Hook: it produces no stored result (returns nil)
// and never mutates the Context. Emitting a Prometheus sample is a cheap,
// non-blocking atomic op, so it satisfies the "Fill must be cheap" rule.
type Hook struct{}

// New constructs the (stateless) metrics observer.
func New() *Hook { return &Hook{} }

func (*Hook) Name() string { return "metrics" }

func (*Hook) Fill(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
	metrics.RecordServed(lc.Source, ev.Status, lc.Timing.End, upstreamDuration(lc))
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

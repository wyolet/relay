package usagelog

import (
	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// EchoFromContext maps the usage Event that UsageHook attached to lc into
// the caller-facing v1.RelayUsage block. Returns nil when no usage Event
// was collected (so the response-writer simply doesn't inject anything).
//
// This is the read side of the blackboard: the producer (UsageHook) filled
// the Event once; echo reads that same collection here, the sink reads it
// at Finalize — no re-parse. Deliberately drops operator-only attribution
// (relay-key hash, policy/host/model UUIDs); the caller gets observability,
// not internals.
func EchoFromContext(lc *lifecycle.Context) *v1.RelayUsage {
	v, ok := lc.Collected(Namespace)
	if !ok {
		return nil
	}
	ev, ok := v.(*Event)
	if !ok || ev == nil {
		return nil
	}
	ru := &v1.RelayUsage{
		RequestID:    ev.RequestID,
		Tokens:       ev.Tokens,
		FinishReason: v1.FinishReason(ev.FinishReason),
		Attempts:     ev.Attempts,
		Streamed:     ev.Streamed,
		// End comes from lc.Timing (full-precision duration → µs); the
		// Event only carries the ms total. Upstream marks are already µs
		// on the Event.
		Timing: &v1.RelayTiming{End: lc.Timing.End.Microseconds()},
	}
	if ev.Upstream != nil {
		ru.Timing.Upstream = v1.RelayUpstreamTiming{
			Start:         ev.Upstream.Start,
			ResponseStart: ev.Upstream.ResponseStart,
			ResponseEnd:   ev.Upstream.ResponseEnd,
		}
	}
	return ru
}

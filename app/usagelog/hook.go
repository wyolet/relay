package usagelog

import (
	"context"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

// HookOptions has no required dependencies — the canonical-first hook
// reads everything from the lifecycle.Context (Translator, routing
// identity) and the PostFlightEvent (ResponseBody). Struct is kept
// so future toggles (sampling, redaction) can be added without
// breaking the constructor signature.
type HookOptions struct{}

// Hook is the lifecycle.PostFlightHook implementation. Construct at
// boot, register on lifecycle.Registry via RegisterPostFlight.
//
// Pure canonical observer: calls v1.ExtractUsage on the buffered
// response body using the per-request Translator the runner set on
// lc, builds an Event, queues it. No vendor-specific JSON parsing,
// no gzip/SSE branching at this layer — that lives in pkg/relay/v1
// where canonical lives.
type Hook struct {
	opts    HookOptions
	emitter *Emitter
}

// NewHook constructs a Hook that pushes built Events onto e.
func NewHook(opts HookOptions, e *Emitter) *Hook {
	return &Hook{opts: opts, emitter: e}
}

// PostFlight is the lifecycle.PostFlightHook entry point.
func (h *Hook) PostFlight(_ context.Context, lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) {
	out := Event{
		RequestID:      lc.RequestID,
		Source:         lc.Source,
		Timestamp:      lc.Timing.Start,
		Status:         ev.Status,
		DurationMs:     lc.Timing.End.Milliseconds(),
		Streamed:       lc.Streamed,
		Attempts:       lc.Attempts,
		ErrorKind:      ev.ErrorKind,
		ErrorMessage:   ev.ErrorMessage,
		RelayKeyHash:   lc.RelayKeyHash,
		PolicyID:       lc.PolicyID,
		ModelID:        lc.ModelID,
		RequestedModel: lc.RequestedModel,
		HostID:         lc.HostID,
		HostKeyID:      lc.HostKeyID,
	}
	if out.Timestamp.IsZero() {
		out.Timestamp = time.Now()
	}

	// Upstream-leg breakdown, microseconds from start. Present only when
	// the request actually reached upstream (Start mark stamped).
	if up := lc.Timing.Upstream; up.Start > 0 {
		out.Upstream = &UpstreamTiming{
			Start:         up.Start.Microseconds(),
			ResponseStart: up.ResponseStart.Microseconds(),
			ResponseEnd:   up.ResponseEnd.Microseconds(),
		}
	}

	if lc.Translator != nil && len(ev.ResponseBody) > 0 {
		if s, err := v1.ExtractSummary(lc.Translator, ev.ResponseBody); err == nil {
			out.Tokens = s.Tokens
			out.FinishReason = string(s.FinishReason)
		}
	}

	if len(lc.Metadata) > 0 {
		extras := make(map[string]string, len(lc.Metadata))
		for k, v := range lc.Metadata {
			if s, ok := v.(string); ok {
				extras[k] = s
			}
		}
		if len(extras) > 0 {
			out.Extras = extras
		}
	}

	h.emitter.Emit(out)
}

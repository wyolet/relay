package usagelog

import (
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

// Namespace is the key under which UsageHook attaches its Event to the
// lifecycle Context. SinkCollector (store side) and any pre-send reader
// (usage echo) read it back via lc.Collected(Namespace).
const Namespace = "usage"

// UsageHook is the usage producer: a lifecycle.Hook that builds the
// canonical per-request Event from the Context's identity/timing and the
// PostFlightEvent's outcome. Pure — it never touches the Context; the
// Registry attaches the returned *Event under Namespace. Storing happens
// later, in SinkCollector.
//
// Canonical-first: token counts + finish reason come from
// v1.ExtractSummary on the buffered body using the per-request
// Translator — no vendor-specific JSON/SSE branching at this layer.
type UsageHook struct{}

// NewUsageHook constructs the (stateless) usage producer.
func NewUsageHook() *UsageHook { return &UsageHook{} }

func (*UsageHook) Name() string { return Namespace }

// Fill builds the Event. Always returns one (error rows are valid usage
// records), so every finalized request yields exactly one row.
func (*UsageHook) Fill(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
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
			if str, ok := v.(string); ok {
				extras[k] = str
			}
		}
		if len(extras) > 0 {
			out.Extras = extras
		}
	}

	return &out, nil
}

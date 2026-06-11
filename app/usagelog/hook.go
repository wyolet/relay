package usagelog

import (
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/sdk/v1"
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
type UsageHook struct {
	pricer *Pricer
}

// NewUsageHook constructs the usage producer. pricer may be nil (events
// stay unpriced).
func NewUsageHook(pricer *Pricer) *UsageHook { return &UsageHook{pricer: pricer} }

func (*UsageHook) Name() string { return Namespace }

// Fill builds the Event. Always returns one (error rows are valid usage
// records), so every finalized request yields exactly one row.
func (h *UsageHook) Fill(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
	return buildEvent(lc, ev.Status, ev.ErrorKind, ev.ErrorMessage, ev.ResponseBody, h.pricer), nil
}

// buildEvent assembles the canonical usage Event from the per-request
// Context plus this-request outcome (status/error/body). Shared by the
// post-flight UsageHook and the streaming observer (which passes the
// accumulated stream bytes as body); both land the same Event under the
// same Namespace so echo + sink see one shape regardless of stream vs
// buffered.
func buildEvent(lc *lifecycle.Context, status int, errKind, errMsg string, body []byte, pricer *Pricer) *Event {
	out := Event{
		RequestID:      lc.RequestID,
		Source:         lc.Source,
		Timestamp:      lc.Timing.Start,
		Status:         status,
		DurationMs:     lc.Timing.End.Milliseconds(),
		Streamed:       lc.Streamed,
		Attempts:       lc.Attempts,
		ErrorKind:      errKind,
		ErrorMessage:   errMsg,
		RelayKeyHash:   lc.RelayKeyHash,
		PolicyID:       lc.PolicyID,
		ModelID:        lc.ModelID,
		RequestedModel: lc.RequestedModel,
		HostID:         lc.HostID,
		HostKeyID:      lc.HostKeyID,
		Model:          lc.ModelName,
		Host:           lc.HostName,
		Policy:         lc.PolicyName,
		Provider:       lc.ProviderName,
		Pricing:        lc.PricingName,
	}
	if !lc.EventTime.IsZero() {
		out.Timestamp = lc.EventTime
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

	// Reasoning span, microseconds from start. Present only when a
	// reasoning frame was observed on the canonical stream.
	if rz := lc.Timing.Reasoning; rz.Start > 0 {
		out.Reasoning = &ReasoningTiming{
			Start: rz.Start.Microseconds(),
			End:   rz.End.Microseconds(),
		}
	}

	if lc.Translator != nil && len(body) > 0 {
		if s, err := v1.ExtractSummary(lc.Translator, body); err == nil {
			out.Tokens = s.Tokens
			out.FinishReason = string(s.FinishReason)
		}
	}

	// Emit-time cost: priced only when a rate sheet was stamped AND tokens
	// matched its meters — anything else stays unpriced (CostNanos nil),
	// never a fabricated $0. The Pricing slug above is stamped regardless,
	// recording which sheet covered the route.
	if nanos, breakdown, ok := pricer.Price(lc.PricingID, out.Tokens); ok {
		out.CostNanos = &nanos
		out.CostBreakdown = breakdown
	}

	if len(lc.Metadata) > 0 {
		extras := make(map[string]string, len(lc.Metadata))
		for k, v := range lc.Metadata {
			if k == MetadataKeyRequestTags {
				continue // caller tags land on Event.Tags, not relay-stamped Extras
			}
			if str, ok := v.(string); ok {
				extras[k] = str
			}
		}
		if len(extras) > 0 {
			out.Extras = extras
		}
	}

	if raw, ok := lc.Metadata[MetadataKeyRequestTags].(string); ok {
		if tags, ok := ParseTags(raw); ok {
			out.Tags = tags
		}
	}

	return &out
}

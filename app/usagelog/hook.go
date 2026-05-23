package usagelog

import (
	"context"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/usage"
)

// TokenExtractor parses a vendor wire response into usage.Tokens.
// Relay's pipeline.Adapter satisfies this via its ExtractTokens method.
type TokenExtractor interface {
	ExtractTokens(body []byte) usage.Tokens
}

// AdapterResolver returns the TokenExtractor for the upstream adapter
// serving the (modelID, hostID) binding. Adapter selection lives on
// the HostBinding because one Host can serve different wire shapes
// across Models (e.g. Bedrock serves Anthropic shape for Claude and
// OpenAI shape for Llama). Returns (nil, false) when the binding is
// unknown — the hook then emits the event without tokens.
type AdapterResolver interface {
	ExtractorForBinding(modelID, hostID string) (TokenExtractor, bool)
}

// HookOptions wires the Hook's dependencies.
type HookOptions struct {
	// Adapters resolves the per-binding token extractor. Optional —
	// when nil the hook emits events without parsing tokens (still
	// useful for request-trace dashboards / audit).
	Adapters AdapterResolver
}

// Hook is the lifecycle.PostFlightHook implementation. Construct at
// boot with an adapter resolver injected, then register on
// lifecycle.Registry via RegisterPostFlight.
//
// Pure observer: extracts tokens from the response body, builds an
// Event, pushes onto the Emitter queue. No cost, no pricing — those
// are downstream consumer concerns.
type Hook struct {
	opts    HookOptions
	emitter *Emitter
}

// NewHook constructs a Hook that pushes built Events onto e.
func NewHook(opts HookOptions, e *Emitter) *Hook {
	return &Hook{opts: opts, emitter: e}
}

// PostFlight is the lifecycle.PostFlightHook entry point. Runs in a
// parallel goroutine from the runner's post-flight chain.
func (h *Hook) PostFlight(_ context.Context, lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) {
	out := Event{
		RequestID:    lc.RequestID,
		Source:       lc.Source,
		Timestamp:    lc.StartTime.Add(ev.Duration),
		Status:       ev.Status,
		DurationMs:   ev.Duration.Milliseconds(),
		ErrorKind:    ev.ErrorKind,
		ErrorMessage: ev.ErrorMessage,
		RelayKeyHash: lc.RelayKeyHash,
		PolicyID:     lc.PolicyID,
		ModelID:      lc.ModelID,
		HostID:       lc.HostID,
		HostKeyID:    lc.HostKeyID,
	}
	if ev.Duration == 0 {
		out.Timestamp = time.Now()
	}

	if h.opts.Adapters != nil && len(ev.ResponseBody) > 0 && lc.ModelID != "" && lc.HostID != "" {
		if ext, ok := h.opts.Adapters.ExtractorForBinding(lc.ModelID, lc.HostID); ok && ext != nil {
			out.Tokens = ext.ExtractTokens(ev.ResponseBody)
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

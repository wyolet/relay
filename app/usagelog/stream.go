package usagelog

import (
	"net/http"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// StreamUsageFactory is the streaming counterpart to UsageHook: a
// lifecycle.StreamObserverFactory whose observer builds the same usage Event
// via the shared buildEventWithSummary at end-of-stream, from the Summary the
// StreamSession harvested frame-by-frame. Streamed requests therefore collect
// usage exactly like buffered ones — same Event, same Namespace — so echo and
// the sink both see one shape, without retaining the stream. The post-flight
// Hook is too late for a streamed body (it runs after the stream is already
// written); this fills during the stream.
type StreamUsageFactory struct {
	pricer   *Pricer
	instance string
}

// NewStreamUsageFactory constructs the factory. pricer may be nil (events
// stay unpriced); instanceID may be empty (no instance stamp).
func NewStreamUsageFactory(pricer *Pricer, instanceID string) *StreamUsageFactory {
	return &StreamUsageFactory{pricer: pricer, instance: instanceID}
}

func (*StreamUsageFactory) Name() string { return Namespace }

func (f *StreamUsageFactory) NewObserver(lc *lifecycle.Context) lifecycle.StreamObserver {
	return &streamUsageObserver{lc: lc, pricer: f.pricer, instance: f.instance}
}

type streamUsageObserver struct {
	lc       *lifecycle.Context
	pricer   *Pricer
	instance string
}

// Observe is a no-op: the session's own summarizer already translates every
// frame, and running a second one doubled the per-frame cost.
func (*streamUsageObserver) Observe([]byte) {}

func (o *streamUsageObserver) Result() (any, error) {
	s, _ := o.lc.StreamSummary()
	// The post-flight event never reaches a stream observer, so the status comes from what the runner stamped on the Context.
	status := o.lc.ResponseStatus
	if status == 0 {
		status = http.StatusOK
	}
	out := buildEventWithSummary(o.lc, status, "", "", s, o.pricer)
	stampInstance(out, o.instance)
	return out, nil
}

package usagelog

import (
	"bytes"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// StreamUsageFactory is the streaming counterpart to UsageHook: a
// lifecycle.StreamObserverFactory whose observer buffers the raw upstream
// SSE frames and, at end-of-stream, builds the same usage Event via the
// shared buildEvent (which runs v1.ExtractSummary on the accumulated
// body). Streamed requests therefore collect usage exactly like buffered
// ones — same Event, same Namespace — so echo and the sink both see one
// shape. The post-flight Hook is too late for a streamed body (it runs
// after the stream is already written); this fills during the stream.
type StreamUsageFactory struct {
	pricer *Pricer
}

// NewStreamUsageFactory constructs the factory. pricer may be nil (events
// stay unpriced).
func NewStreamUsageFactory(pricer *Pricer) *StreamUsageFactory {
	return &StreamUsageFactory{pricer: pricer}
}

func (*StreamUsageFactory) Name() string { return Namespace }

func (f *StreamUsageFactory) NewObserver(lc *lifecycle.Context) lifecycle.StreamObserver {
	return &streamUsageObserver{lc: lc, pricer: f.pricer}
}

// streamUsageObserver accumulates upstream frames for one streamed request.
// A streamed response that began is a success (status 200) with no error.
type streamUsageObserver struct {
	lc     *lifecycle.Context
	pricer *Pricer
	buf    bytes.Buffer
}

// Observe re-appends the SSE frame separator the dispatch scanner strips,
// so the accumulated buffer is a faithful SSE body for ExtractSummary.
func (o *streamUsageObserver) Observe(frame []byte) {
	o.buf.Write(frame)
	o.buf.WriteString("\n\n")
}

func (o *streamUsageObserver) Result() (any, error) {
	return buildEvent(o.lc, 200, "", "", o.buf.Bytes(), o.pricer), nil
}

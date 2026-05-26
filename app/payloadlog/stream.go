package payloadlog

import (
	"bytes"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// StreamPayloadFactory is the streamed-path counterpart to PayloadHook.
// Its observer accumulates the raw upstream SSE frames and builds the
// Record at end-of-stream (the buffered hook is too late — it runs after
// the stream is written). When capture is disabled for the request it
// returns a no-op observer so non-logged streams never buffer.
type StreamPayloadFactory struct {
	maxBytes int
}

// NewStreamPayloadFactory constructs the factory with a per-body cap.
func NewStreamPayloadFactory(maxBytes int) *StreamPayloadFactory {
	return &StreamPayloadFactory{maxBytes: maxBytes}
}

func (*StreamPayloadFactory) Name() string { return Namespace }

func (f *StreamPayloadFactory) NewObserver(lc *lifecycle.Context) lifecycle.StreamObserver {
	if lc == nil || !lc.PayloadLog {
		return noopObserver{}
	}
	return &streamPayloadObserver{lc: lc, max: f.maxBytes}
}

// streamPayloadObserver accumulates upstream frames for one opted-in
// streamed request. A streamed response that began is a success (200).
type streamPayloadObserver struct {
	lc  *lifecycle.Context
	max int
	buf bytes.Buffer
}

// Observe re-appends the SSE frame separator the dispatch scanner strips,
// so the accumulated buffer is a faithful response body. Stops growing
// once the cap is reached (the Record is flagged truncated by buildRecord).
func (o *streamPayloadObserver) Observe(frame []byte) {
	if o.max > 0 && o.buf.Len() >= o.max {
		return
	}
	o.buf.Write(frame)
	o.buf.WriteString("\n\n")
}

func (o *streamPayloadObserver) Result() (any, error) {
	return buildRecord(o.lc, 200, "", o.buf.Bytes(), o.max), nil
}

// noopObserver is returned for non-opted-in streams: it ignores frames
// and attaches nothing, so the collector skips the request.
type noopObserver struct{}

func (noopObserver) Observe([]byte)       {}
func (noopObserver) Result() (any, error) { return nil, nil }

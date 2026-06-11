package payloadlog

import (
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// PayloadHook is the buffered-path producer: a lifecycle.Hook that builds
// a Record from the request body retained on the Context and the response
// body on the PostFlightEvent. Returns nil (nothing attached) when the
// request isn't opted into capture, so non-logged requests cost nothing
// beyond the gate check.
//
// The Controller supplies the live master switch + per-body cap, both
// runtime-mutable via settings.
type PayloadHook struct {
	c *Controller
}

// NewPayloadHook constructs the producer bound to the Controller.
func NewPayloadHook(c *Controller) *PayloadHook { return &PayloadHook{c: c} }

func (*PayloadHook) Name() string { return Namespace }

func (h *PayloadHook) Fill(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
	if lc == nil || !lc.PayloadLog || !h.c.Enabled() {
		return nil, nil
	}
	return buildRecord(lc, ev.ResponseBody, h.c.MaxBytes()), nil
}

// buildRecord assembles the body-only Record (request_id + ts + bodies +
// truncation flags) — all per-request metadata lives on the log event, not
// here. Shared by the buffered hook and the streaming observer (which passes
// the accumulated stream bytes as body). Callers must have checked
// lc.PayloadLog.
func buildRecord(lc *lifecycle.Context, respBody []byte, maxBytes int) *Record {
	ts := lc.Timing.Start
	if ts.IsZero() {
		ts = time.Now()
	}
	reqBody, reqTrunc := clip(lc.RequestBody, maxBytes)
	// The proxy peek-then-stream path retains only a body prefix; the record
	// must flag that even when MaxBytes itself didn't clip anything.
	reqTrunc = reqTrunc || lc.RequestBodyTruncated
	respBody, respTrunc := clip(respBody, maxBytes)
	return &Record{
		RequestID:         lc.RequestID,
		Timestamp:         ts,
		RequestBody:       reqBody,
		ResponseBody:      respBody,
		RequestTruncated:  reqTrunc,
		ResponseTruncated: respTrunc,
	}
}

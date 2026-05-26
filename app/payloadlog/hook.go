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
// maxBytes caps each stored body (0 = unlimited); oversized bodies are
// truncated and flagged on the Record.
type PayloadHook struct {
	maxBytes int
}

// NewPayloadHook constructs the producer with a per-body storage cap.
func NewPayloadHook(maxBytes int) *PayloadHook { return &PayloadHook{maxBytes: maxBytes} }

func (*PayloadHook) Name() string { return Namespace }

func (h *PayloadHook) Fill(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
	if lc == nil || !lc.PayloadLog {
		return nil, nil
	}
	return buildRecord(lc, ev.Status, ev.ErrorKind, ev.ResponseBody, h.maxBytes), nil
}

// buildRecord assembles a Record from the per-request Context plus this
// request's outcome. Shared by the buffered hook and the streaming
// observer (which passes the accumulated stream bytes as body). Callers
// must have checked lc.PayloadLog.
func buildRecord(lc *lifecycle.Context, status int, errKind string, respBody []byte, maxBytes int) *Record {
	ts := lc.Timing.Start
	if ts.IsZero() {
		ts = time.Now()
	}
	reqBody, reqTrunc := clip(lc.RequestBody, maxBytes)
	respBody, respTrunc := clip(respBody, maxBytes)
	return &Record{
		RequestID:         lc.RequestID,
		Timestamp:         ts,
		Source:            lc.Source,
		Status:            status,
		Streamed:          lc.Streamed,
		RelayKeyHash:      lc.RelayKeyHash,
		PolicyID:          lc.PolicyID,
		ModelID:           lc.ModelID,
		HostID:            lc.HostID,
		ErrorKind:         errKind,
		RequestBody:       reqBody,
		ResponseBody:      respBody,
		RequestTruncated:  reqTrunc,
		ResponseTruncated: respTrunc,
	}
}

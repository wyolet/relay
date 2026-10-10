package pipeline

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// drainTimeout bounds how long post-flight keeps reading an upstream response
// the caller stopped reading, and how long the upstream outlives a caller that
// went away after the response started. Long enough for a usage frame that
// trails the content; short enough that an abandoned generation is cut off
// rather than run to completion on the operator's key.
const drainTimeout = 2 * time.Second

// maxDrainBytes bounds the unread tail post-flight reads after an early close.
const maxDrainBytes = 1 << 20

// bytesPerEstimatedToken matches the divisor the count-tokens endpoint falls
// back to when it has nothing better.
const bytesPerEstimatedToken = 4

// upstreamCall is the context one Adapter.Call runs under. Until a response is
// accepted it is cancelled with the caller's context, so a caller leaving
// mid-call aborts the upstream at once. After that the caller's cancellation
// only cuts the upstream drainTimeout later, so the response tail (where usage
// lives) can still be read for accounting.
type upstreamCall struct {
	ctx      context.Context
	cancel   context.CancelFunc
	unlink   func() bool
	accepted atomic.Bool
}

func newUpstreamCall(parent context.Context) *upstreamCall {
	c := &upstreamCall{}
	c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(parent))
	c.unlink = context.AfterFunc(parent, func() {
		if c.accepted.Load() {
			time.AfterFunc(drainTimeout, c.cancel)
			return
		}
		c.cancel()
	})
	return c
}

// release ends the call: the caller-cancellation link is dropped and the
// upstream request context cancelled. Idempotent.
func (c *upstreamCall) release() {
	c.unlink()
	c.cancel()
}

// readState wraps the response reader and records how reading ended. Read by
// one goroutine at a time: the caller, then (after Close) post-flight.
type readState struct {
	r    io.Reader
	done bool // a Read returned an error (EOF included)
	eof  bool // the response was read to its end
}

func (s *readState) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil {
		s.done = true
		s.eof = err == io.EOF
	}
	return n, err
}

// drainTail reads what the caller left unread, bounded by maxDrainBytes and
// drainTimeout, so a usage frame at the end of the response still reaches the
// tee. Reports whether the response was read to its end.
func drainTail(s *readState, call *upstreamCall) bool {
	t := time.AfterFunc(drainTimeout, call.cancel)
	defer t.Stop()
	_, _ = io.Copy(io.Discard, io.LimitReader(s, maxDrainBytes))
	return s.eof
}

// countSSEFrames counts the blank-line-terminated SSE frames in body; a
// trailing unterminated frame (or a body that is not SSE) counts as none.
func countSSEFrames(body []byte) int {
	frames := 0
	for {
		n, frame, _ := v1.SplitSSEFrames(body, false)
		if n == 0 {
			return frames
		}
		if frame != nil {
			frames++
		}
		body = body[n:]
	}
}

// withUnreportedFloor returns the tokens to charge a response that ended
// before the upstream reported its usage: input not reported is estimated
// from the request size, and output is at least one token per frame seen.
// Reported counts that are higher are kept.
func withUnreportedFloor(t sdkusage.Tokens, requestBytes, frames int) sdkusage.Tokens {
	out := make(sdkusage.Tokens, len(t)+2)
	for k, v := range t {
		out[k] = v
	}
	if out["input"] == 0 {
		out["input"] = int64(requestBytes / bytesPerEstimatedToken)
	}
	if n := int64(frames); out["output"] < n {
		out["output"] = n
	}
	return out
}

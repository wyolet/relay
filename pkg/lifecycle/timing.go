package lifecycle

import (
	"io"
	"time"
)

// Timing holds per-request checkpoints. Start is the absolute anchor;
// every other field is elapsed time measured from Start. All anchored to
// Start, never chained — so a missing or flaky intermediate mark can't
// corrupt the others, and the headline numbers (TTFT, total) are read
// directly rather than summed up a chain that compounds error.
//
// The unit lives here, once: the elapsed fields are time.Duration in
// memory; sinks serialize them to microseconds. Derived intervals are
// computed by the consumer, never stored:
//
//	relay pre-overhead = Upstream.Start
//	upstream TTFT      = Upstream.ResponseStart - Upstream.Start
//	stream body time   = Upstream.ResponseEnd   - Upstream.ResponseStart
//	relay tail         = End                    - Upstream.ResponseEnd
type Timing struct {
	Start    time.Time      // request accepted (absolute anchor)
	Upstream UpstreamTiming // the upstream leg
	End      time.Duration  // start → response closed / post-flight
}

// UpstreamTiming groups the upstream-leg checkpoints, each elapsed from
// Timing.Start.
type UpstreamTiming struct {
	Start         time.Duration // request handed to upstream
	ResponseStart time.Duration // first upstream byte received (TTFT mark)
	ResponseEnd   time.Duration // upstream finished sending
}

// sinceStart is the elapsed time from the anchor. Caller guards nil.
func (c *Context) sinceStart() time.Duration { return time.Since(c.Timing.Start) }

// MarkUpstreamStart records the moment the request is handed to upstream.
// Called once per attempt by the runner immediately before the upstream
// call; the successful attempt's value is the one that survives. Nil-safe.
func (c *Context) MarkUpstreamStart() {
	if c != nil {
		c.Timing.Upstream.Start = c.sinceStart()
	}
}

// MarkEnd records request completion (response closed / post-flight
// dispatch). Called by the runner in the post-flight goroutine. Nil-safe.
func (c *Context) MarkEnd() {
	if c != nil {
		c.Timing.End = c.sinceStart()
	}
}

// FirstByteReader wraps r so the first non-empty read stamps the upstream
// response-start (TTFT) mark and EOF stamps the response-end mark. The
// runner wraps the tee'd upstream body with this; marks land as the
// caller drains the stream. Nil-safe — returns r unwrapped when c is nil.
func (c *Context) FirstByteReader(r io.Reader) io.Reader {
	if c == nil {
		return r
	}
	return &firstByteReader{r: r, c: c}
}

type firstByteReader struct {
	r    io.Reader
	c    *Context
	seen bool
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && !f.seen {
		f.seen = true
		f.c.Timing.Upstream.ResponseStart = f.c.sinceStart()
	}
	if err == io.EOF {
		f.c.Timing.Upstream.ResponseEnd = f.c.sinceStart()
	}
	return n, err
}

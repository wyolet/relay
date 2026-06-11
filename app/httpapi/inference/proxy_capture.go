package inference

import (
	"bytes"
	"io"
	"sync"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// bodyCapture accumulates the full request body of a streamed proxy
// forward for payload logging while the same bytes flow upstream
// untouched. It fills on the transport's write goroutine as upstream
// consumes the body; the post-flight observers read lc.RequestBody from
// yet another goroutine. The handoff is made explicit rather than
// relying on HTTP ordering: finalizeInto publishes a consistent
// snapshot under the capture mutex, and handleProxy invokes it from the
// response body's Close BEFORE the inner Close spawns the detached
// post-flight goroutine — the `go` statement is the happens-before edge.
// Observers therefore see either the complete capture or a deliberate
// partial prefix flagged truncated, never a torn buffer.
type bodyCapture struct {
	mu sync.Mutex
	// buf is seeded with the peeked prefix via bytes.NewBuffer — zero
	// copy. Appends land beyond len(prefix), so the prefix slice that
	// seedLifecycle published as the pre-finalize fallback is never
	// overwritten even when growth happens in place.
	buf       *bytes.Buffer
	limit     int
	overflow  bool // hit limit; upstream keeps streaming, capture stops
	complete  bool // remainder reached EOF — buf holds the whole body
	finalized bool
}

func newBodyCapture(prefix []byte, limit int) *bodyCapture {
	return &bodyCapture{buf: bytes.NewBuffer(prefix), limit: limit}
}

// tee returns the reader to forward upstream in place of rest: bytes
// pass through unmodified while a side-copy accumulates, so the
// upstream send never waits on anything beyond an in-memory append.
func (c *bodyCapture) tee(rest io.Reader) io.Reader {
	return &captureTee{r: rest, c: c}
}

func (c *bodyCapture) observe(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finalized || c.overflow {
		return
	}
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.overflow = true
		return
	}
	if len(p) > room {
		p = p[:room]
		c.overflow = true
	}
	c.buf.Write(p)
}

func (c *bodyCapture) markComplete() {
	c.mu.Lock()
	c.complete = true
	c.mu.Unlock()
}

// finalizeInto publishes the assembled body onto lc. Incomplete means
// upstream answered before draining the request body — the snapshot is
// still a consistent prefix, flagged truncated. After finalize the
// capture stops accepting writes, so the published slice is stable.
// Callers must order this before the post-flight goroutine spawns (see
// finalizeReadCloser).
func (c *bodyCapture) finalizeInto(lc *lifecycle.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalized = true
	lc.RequestBody = c.buf.Bytes()
	lc.RequestBodyTruncated = c.overflow || !c.complete
}

type captureTee struct {
	r io.Reader
	c *bodyCapture
}

func (t *captureTee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.c.observe(p[:n])
	}
	if err == io.EOF {
		t.c.markComplete()
	}
	return n, err
}

// finalizeReadCloser orders the capture handoff: Close publishes the
// capture into lc first, then closes inner — whose closer is what
// spawns the post-flight goroutine.
type finalizeReadCloser struct {
	inner io.ReadCloser
	c     *bodyCapture
	lc    *lifecycle.Context
	once  sync.Once
}

func (f *finalizeReadCloser) Read(p []byte) (int, error) { return f.inner.Read(p) }

func (f *finalizeReadCloser) Close() error {
	f.once.Do(func() { f.c.finalizeInto(f.lc) })
	return f.inner.Close()
}

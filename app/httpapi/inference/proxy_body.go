package inference

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// proxyHoldBytes sizes the first read of a proxy body; a typical chat body arrives whole in it.
const proxyHoldBytes = 32 << 10

// maxHeldTail caps the bytes held from the top-level close on: whitespace a client may send after the object, held until its body ends.
const maxHeldTail = 1 << 10

var (
	errProxyBodyTooLarge = &bodyError{http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body has no top-level 'model' in its first %d MiB", proxyMaxBodyBuffer>>20)}
	errTrailingBytes     = jsonSyntaxError("more than %d bytes after the top-level value", maxHeldTail)
	errForwardStalled    = &bodyError{http.StatusBadGateway, "upstream_stalled", "the request body stopped moving: the upstream stopped reading it, or the client stopped sending"}
)

// scannedBody forwards a proxy request body upstream while a modelScanner checks it. Each chunk read from the client is scanned before it is released, and the bytes from the top-level object's closing brace on are held until the client's body ends, so the upstream receives a complete JSON body only once all of it passed the scan. A rejected body fails the read, which makes the transport abort the upstream request with the body incomplete.
//
// Read runs on the transport's goroutine; the atomics and the fields under mu are what other goroutines read.
type scannedBody struct {
	scan    *modelScanner
	src     io.Reader
	pending []byte // scanned, not yet released
	held    []byte // from the top-level close on, released at the end of the body
	srcDone bool
	readErr error
	whole   bool // the client's body ended before the forward started
	capture *bodyCapture

	total   atomic.Int64 // bytes read from the client so far
	drained atomic.Bool  // the client's body was read to its end
	stopped atomic.Bool  // the body was refused: no more reads of the client

	mu       sync.Mutex
	refusal  *proxy.BodyError
	model    string // the body's model once scanned
	complete bool   // the body was read to its end and passed the scan
	size     int64  // the body's length, once complete
}

// holdProxyBody reads body until its top-level model is known and returns the bytes read with the scannedBody that forwards them and then the rest. A model after the messages makes it hold the whole body, up to proxyMaxBodyBuffer. With needModel false nothing is read here; the scan runs as the body streams.
func holdProxyBody(body io.Reader, contentLength int64, needModel bool) (*scannedBody, []byte, *bodyError) {
	scan := &modelScanner{exactKey: true}
	if !needModel {
		return newScannedBody(scan, nil, -1, body), nil, nil
	}
	size := proxyHoldBytes
	if contentLength > 0 && contentLength < proxyHoldBytes {
		size = int(contentLength)
	}
	buf := make([]byte, 0, size)
	closeAt := -1
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, holdCapacity(cap(buf), contentLength)-len(buf))
		}
		n, err := body.Read(buf[len(buf):min(cap(buf), proxyMaxBodyBuffer+1)])
		if at := scan.feed(buf[len(buf) : len(buf)+n]); at >= 0 {
			closeAt = len(buf) + at
		}
		buf = buf[:len(buf)+n]
		switch {
		case scan.err != nil:
			return nil, nil, scan.err
		case len(buf) > proxyMaxBodyBuffer:
			return nil, nil, errProxyBodyTooLarge
		case err == io.EOF:
			if ferr := scan.finish(); ferr != nil {
				return nil, nil, ferr
			}
			sb := newScannedBody(scan, buf, closeAt, nil)
			sb.markWhole()
			return sb, buf, nil
		case err != nil:
			return nil, nil, sourceBodyError(err)
		case scan.modelDone && scan.model == "":
			return nil, nil, errMissingModel
		case scan.modelDone:
			return newScannedBody(scan, buf, closeAt, body), buf, nil
		}
	}
}

// holdCapacity is the next size of a full hold buffer of size c: the declared length (one byte over, so the read that sees the end has room) when that is within maxUpfrontBodyBytes or c's double, else c's double, never past one byte over proxyMaxBodyBuffer.
func holdCapacity(c int, declared int64) int {
	next := int64(2 * c)
	if declared >= int64(c) {
		next = min(declared+1, max(next, maxUpfrontBodyBytes))
	}
	return int(min(next, proxyMaxBodyBuffer+1))
}

// scannedWhole checks a body already read in full.
func scannedWhole(body []byte) (*scannedBody, *bodyError) {
	scan := &modelScanner{exactKey: true}
	closeAt := scan.feed(body)
	if err := scan.finish(); err != nil {
		return nil, err
	}
	sb := newScannedBody(scan, body, closeAt, nil)
	sb.markWhole()
	return sb, nil
}

// newScannedBody continues scan over src after prefix, which scan has already been fed; closeAt is what that feed returned.
func newScannedBody(scan *modelScanner, prefix []byte, closeAt int, src io.Reader) *scannedBody {
	b := &scannedBody{scan: scan, src: src, pending: prefix, model: scan.model}
	b.total.Store(int64(len(prefix)))
	if closeAt >= 0 {
		b.pending = prefix[:closeAt]
		b.held = prefix[closeAt:len(prefix):len(prefix)]
		if len(b.held) > maxHeldTail {
			b.reject(errTrailingBytes)
		}
	}
	return b
}

func (b *scannedBody) markWhole() {
	b.srcDone, b.whole = true, true
	b.drained.Store(true)
	b.complete, b.size = true, b.total.Load()
}

// captureRest tees the part of the body still to come into a payload capture seeded with prefix, the part already held.
func (b *scannedBody) captureRest(prefix []byte, limit int) {
	b.capture = newBodyCapture(prefix, limit)
	b.src = b.capture.tee(b.src)
}

func (b *scannedBody) Read(p []byte) (int, error) {
	for {
		if len(b.pending) > 0 {
			n := copy(p, b.pending)
			b.pending = b.pending[n:]
			if len(b.pending) == 0 {
				b.pending = nil
			}
			return n, nil
		}
		if b.readErr != nil {
			return 0, b.readErr
		}
		if b.srcDone {
			if len(b.held) == 0 {
				return 0, io.EOF
			}
			b.pending, b.held = b.held, nil
			continue
		}
		if len(p) == 0 {
			return 0, nil
		}
		if b.stopped.Load() {
			b.reject(errForwardStalled)
			continue
		}
		n, err := b.src.Read(p)
		release := n
		if n > 0 {
			b.total.Add(int64(n))
			wasClosed, hadModel := b.scan.state == scanDone, b.scan.modelDone
			closeAt := b.scan.feed(p[:n])
			if b.scan.err != nil {
				b.reject(b.scan.err)
				continue
			}
			if !hadModel && b.scan.modelDone {
				b.publish(false)
			}
			switch {
			case wasClosed:
				release = 0
			case closeAt >= 0:
				release = closeAt
			}
			b.held = append(b.held, p[release:n]...)
			if len(b.held) > maxHeldTail {
				b.reject(errTrailingBytes)
				continue
			}
		}
		switch {
		case err == io.EOF:
			b.srcDone = true
			b.drained.Store(true)
			if ferr := b.scan.finish(); ferr != nil {
				b.reject(ferr)
			} else {
				b.publish(true)
			}
		case err != nil:
			b.reject(sourceBodyError(err))
		}
		if b.readErr == nil && release > 0 {
			return release, nil
		}
	}
}

// Close leaves the client's body alone: the server closes it after the handler, and closing it here would drain a stalled client's remainder on the transport's goroutine, which Run waits for.
func (b *scannedBody) Close() error { return nil }

func (b *scannedBody) publish(complete bool) {
	b.mu.Lock()
	b.model = b.scan.model
	if complete {
		b.complete, b.size = true, b.total.Load()
	}
	b.mu.Unlock()
}

// reject refuses the body on the reading goroutine; the first refusal is the one reported.
func (b *scannedBody) reject(e *bodyError) {
	b.pending, b.held = nil, nil
	b.readErr = b.refuse(e)
}

// refuse records e as the body's refusal unless one is already recorded, and stops further reads; any goroutine may call it. It returns the refusal in force.
func (b *scannedBody) refuse(e *bodyError) *proxy.BodyError {
	b.stopped.Store(true)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refusal == nil {
		b.refusal = &proxy.BodyError{Kind: e.code, Err: e}
	}
	return b.refusal
}

// rejection is the reason the body was refused, or nil while it has not been.
func (b *scannedBody) rejection() *bodyError {
	if r := b.Refusal(); r != nil {
		return r.Err.(*bodyError)
	}
	return nil
}

// Refusal implements proxy.RefusingBody, for a refusal that comes after the upstream answered.
func (b *scannedBody) Refusal() *proxy.BodyError {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refusal
}

// wrapResult returns the response body whose Close first records on lc what the forward learned of the request body: the payload capture, the body's length when Content-Length did not give it, and the model when the forward did not wait for it. Close is ordered before the post-flight goroutine starts.
func (b *scannedBody) wrapResult(body io.ReadCloser, lc *lifecycle.Context) io.ReadCloser {
	if lc == nil || (b.capture == nil && lc.RequestBytes > 0 && lc.RequestedModel != "") {
		return body
	}
	return &finalizeReadCloser{inner: body, sb: b, lc: lc}
}

func (b *scannedBody) finalizeInto(lc *lifecycle.Context) {
	if b.capture != nil {
		b.capture.finalizeInto(lc)
	}
	b.mu.Lock()
	model, complete, size := b.model, b.complete, b.size
	b.mu.Unlock()
	if complete {
		lc.RequestBytes = int(size)
	}
	if lc.RequestedModel == "" {
		lc.RequestedModel = model
	}
}

// sourceBodyError answers a failed read of the client's body as handleShape does, and a read that hit the idle deadline with 408.
func sourceBodyError(err error) *bodyError {
	switch {
	case httpmw.IsBodyTooLargeError(err):
		return &bodyError{http.StatusRequestEntityTooLarge, "request_too_large", err.Error()}
	case errors.Is(err, os.ErrDeadlineExceeded):
		return &bodyError{http.StatusRequestTimeout, "request_timeout", "the request body stalled: no bytes arrived within the idle timeout"}
	}
	return &bodyError{http.StatusBadRequest, "read_body", err.Error()}
}

// idleDeadlineBody bounds each read of a request body through the connection's read deadline, so a client that stops sending mid-body is cut off instead of holding an in-flight slot, a rate-limit lease and an upstream connection. At the body's end the deadline is cleared, so the server's own later reads of the connection do not inherit it; after a failed read it stays, so the server's drain of the rest fails fast too.
type idleDeadlineBody struct {
	body io.Reader
	rc   *http.ResponseController
	idle time.Duration
	off  bool // the body ended or failed, or the connection takes no deadline
	// done is set when the handler returns: the connection may be serving its next request by then, so no deadline may be set on it.
	done atomic.Bool
}

var errHandlerReturned = errors.New("request body read after its handler returned")

func (b *idleDeadlineBody) Read(p []byte) (int, error) {
	if b.done.Load() {
		return 0, errHandlerReturned
	}
	if !b.off && b.rc.SetReadDeadline(time.Now().Add(b.idle)) != nil {
		b.off = true
	}
	n, err := b.body.Read(p)
	switch {
	case err == io.EOF && !b.off:
		_ = b.rc.SetReadDeadline(time.Time{})
		fallthrough
	case err != nil:
		b.off = true
	}
	return n, err
}

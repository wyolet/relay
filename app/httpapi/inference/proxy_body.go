package inference

import (
	"io"
	"net/http"
	"slices"
	"sync"

	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// proxyHoldBytes sizes the first read of a proxy body; a typical chat body arrives whole in it.
const proxyHoldBytes = 32 << 10

var errProxyBodyTooLarge = &bodyError{http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds proxy buffer limit"}

// scannedBody forwards a proxy request body upstream while a modelScanner checks it. Each chunk read from the client is scanned before it is released, and the bytes from the top-level object's closing brace on are held until the client's body ends, so the upstream receives a complete JSON body only once all of it passed the scan. A rejected body fails the read, which makes the transport abort the upstream request with the body incomplete.
//
// Read runs on the transport's goroutine; the fields under mu are what other goroutines read.
type scannedBody struct {
	scan    *modelScanner
	src     io.Reader
	closer  io.Closer
	pending []byte // scanned, not yet released
	held    []byte // from the top-level close on, released at the end of the body
	srcDone bool
	readErr error
	total   int64 // bytes read from the client so far
	whole   bool  // the client's body ended before the forward started
	capture *bodyCapture

	mu       sync.Mutex
	rejected *bodyError
	model    string // the body's model once scanned
	complete bool   // the body was read to its end and passed the scan
	size     int64  // the body's length, once complete
}

// holdProxyBody reads body until its top-level model is known and returns the bytes read with the scannedBody that forwards them and then the rest. A model after the messages makes it hold the whole body, up to proxyMaxBodyBuffer. With needModel false nothing is read here; the scan runs as the body streams.
func holdProxyBody(body io.ReadCloser, contentLength int64, needModel bool) (*scannedBody, []byte, *bodyError) {
	scan := &modelScanner{exactKey: true}
	if !needModel {
		return newScannedBody(scan, nil, -1, body, body), nil, nil
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
			sb := newScannedBody(scan, buf, closeAt, nil, body)
			sb.markWhole()
			return sb, buf, nil
		case err != nil:
			return nil, nil, sourceBodyError(err)
		case scan.modelDone && scan.model == "":
			return nil, nil, errMissingModel
		case scan.modelDone:
			return newScannedBody(scan, buf, closeAt, body, body), buf, nil
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
	sb := newScannedBody(scan, body, closeAt, nil, nil)
	sb.markWhole()
	return sb, nil
}

// newScannedBody continues scan over src after prefix, which scan has already been fed; closeAt is what that feed returned.
func newScannedBody(scan *modelScanner, prefix []byte, closeAt int, src io.Reader, closer io.Closer) *scannedBody {
	b := &scannedBody{scan: scan, src: src, closer: closer, pending: prefix, total: int64(len(prefix)), model: scan.model}
	if closeAt >= 0 {
		b.pending = prefix[:closeAt]
		b.held = prefix[closeAt:len(prefix):len(prefix)]
	}
	return b
}

func (b *scannedBody) markWhole() {
	b.srcDone, b.whole = true, true
	b.complete, b.size = true, b.total
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
		n, err := b.src.Read(p)
		release := n
		if n > 0 {
			b.total += int64(n)
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
		}
		switch {
		case err == io.EOF:
			b.srcDone = true
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

func (b *scannedBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

func (b *scannedBody) publish(complete bool) {
	b.mu.Lock()
	b.model = b.scan.model
	if complete {
		b.complete, b.size = true, b.total
	}
	b.mu.Unlock()
}

func (b *scannedBody) reject(e *bodyError) {
	b.pending, b.held = nil, nil
	b.readErr = &proxy.BodyError{Kind: e.code, Err: e}
	b.mu.Lock()
	b.rejected = e
	b.mu.Unlock()
}

// rejection is the reason the body was refused, or nil while it has not been.
func (b *scannedBody) rejection() *bodyError {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rejected
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

// sourceBodyError answers a failed read of the client's body as handleShape does.
func sourceBodyError(err error) *bodyError {
	if httpmw.IsBodyTooLargeError(err) {
		return &bodyError{http.StatusRequestEntityTooLarge, "request_too_large", err.Error()}
	}
	return &bodyError{http.StatusBadRequest, "read_body", err.Error()}
}

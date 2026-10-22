package inference

import (
	"io"
	"net/http"
	"sync"

	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/httpmw"
)

// scannedBody forwards a proxy request body upstream while a modelScanner checks it. Each chunk read from the client is scanned before it is released, and the bytes from the top-level object's closing brace on are held until the client's body ends, so the upstream receives a complete JSON body only once all of it passed the scan. A rejected body fails the read, which makes the transport abort the upstream request with the body incomplete.
//
// Read runs on the transport's goroutine; rejection is the only state another goroutine reads.
type scannedBody struct {
	scan    *modelScanner
	src     io.Reader
	closer  io.Closer
	pending []byte // scanned, not yet released
	held    []byte // from the top-level close on, released at the end of the body
	srcDone bool
	readErr error

	mu       sync.Mutex
	rejected *bodyError
}

// newScannedBody continues scan over src after prefix, which scan has already been fed; closeAt is what that feed returned.
func newScannedBody(scan *modelScanner, prefix []byte, closeAt int, src io.Reader, closer io.Closer) *scannedBody {
	b := &scannedBody{scan: scan, src: src, closer: closer, pending: prefix}
	if closeAt >= 0 {
		b.pending = prefix[:closeAt]
		b.held = prefix[closeAt:len(prefix):len(prefix)]
	}
	return b
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
			wasClosed := b.scan.state == scanDone
			closeAt := b.scan.feed(p[:n])
			if b.scan.err != nil {
				b.reject(b.scan.err)
				continue
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

func (b *scannedBody) reject(e *bodyError) {
	b.pending, b.held = nil, nil
	b.readErr = &proxy.BodyError{Kind: e.code, Err: e}
	b.mu.Lock()
	b.rejected = e
	b.mu.Unlock()
}

// rejection is the reason the body was refused, or nil while it has not been.
func (b *scannedBody) rejection() *bodyError {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rejected
}

// sourceBodyError answers a failed read of the client's body as handleShape does.
func sourceBodyError(err error) *bodyError {
	if httpmw.IsBodyTooLargeError(err) {
		return &bodyError{http.StatusRequestEntityTooLarge, "request_too_large", err.Error()}
	}
	return &bodyError{http.StatusBadRequest, "read_body", err.Error()}
}

// Package reqbody builds outbound HTTP request bodies over an in-memory buffer that stop referencing the buffer once the transport is done with it.
//
// A *bytes.Reader body stays reachable from the client transport (and from Response.Request) for as long as the response is read, so a large request stays pinned for the whole length of a long stream. These readers drop the buffer at EOF or Close instead. Streamed bodies of unknown size are out of scope.
package reqbody

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

var (
	errClosed   = errors.New("reqbody: read after close")
	errReleased = errors.New("reqbody: body released")
)

// NewReader returns a reader over b that drops b once read to the end or closed.
func NewReader(b []byte) io.ReadCloser {
	return &reader{b: b}
}

// Attach sets b as req's body with its Content-Length and a GetBody for transport retries, as http.NewRequest does for a *bytes.Reader. The returned release drops GetBody's reference to b; call it once the response is in hand, after which no transport retry can rewind the body.
func Attach(req *http.Request, b []byte) (release func()) {
	req.ContentLength = int64(len(b))
	if len(b) == 0 {
		req.Body = http.NoBody
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return func() {}
	}
	src := &source{b: b}
	req.Body = NewReader(b)
	req.GetBody = src.open
	return src.release
}

type source struct {
	mu sync.Mutex
	b  []byte
}

func (s *source) open() (io.ReadCloser, error) {
	s.mu.Lock()
	b := s.b
	s.mu.Unlock()
	if b == nil {
		return nil, errReleased
	}
	return NewReader(b), nil
}

func (s *source) release() {
	s.mu.Lock()
	s.b = nil
	s.mu.Unlock()
}

// reader is locked because the transport may Close a request body from another goroutine while its writer still reads it.
type reader struct {
	mu     sync.Mutex
	b      []byte
	off    int
	closed bool
}

func (r *reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errClosed
	}
	if r.off >= len(r.b) {
		r.b = nil
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	if r.off == len(r.b) {
		r.b, r.off = nil, 0
	}
	return n, nil
}

func (r *reader) Close() error {
	r.mu.Lock()
	r.b, r.off, r.closed = nil, 0, true
	r.mu.Unlock()
	return nil
}

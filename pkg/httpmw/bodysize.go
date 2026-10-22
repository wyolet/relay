// Package httpmw holds net/http middleware and helpers shared by relay's listeners: request body size limits, client IP resolution behind trusted proxies, and browser security headers.
package httpmw

import (
	"errors"
	"io"
	"net/http"
)

// DefaultMaxRequestBytes matches Anthropic's documented /v1/messages limit (32 MB).
// OpenAI/Gemini limits are similar or higher; we standardize on this so relay
// never rejects a body the upstream would accept. Override with RELAY_MAX_REQUEST_BYTES.
const DefaultMaxRequestBytes int64 = 32 * 1024 * 1024

// LimitBody fails reads past limit with *http.MaxBytesError. Unlike http.MaxBytesReader it never touches the ResponseWriter: a proxied body is read on the upstream transport's goroutine, where marking the response would race the handler's own header writes. The server drains or closes a connection whose body a handler left unread, as before.
func LimitBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = &limitedBody{body: r.Body, left: limit, limit: limit}
			next.ServeHTTP(w, r)
		})
	}
}

type limitedBody struct {
	body  io.ReadCloser
	left  int64
	limit int64
	err   error
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// One byte past what is left tells a body that ends at the limit from one that goes over.
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.body.Read(p)
	if int64(n) <= b.left {
		b.left -= int64(n)
		b.err = err
		return n, err
	}
	n, b.left = int(b.left), 0
	b.err = &http.MaxBytesError{Limit: b.limit}
	return n, b.err
}

func (b *limitedBody) Close() error { return b.body.Close() }

func IsBodyTooLargeError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

package httpmw

import (
	"errors"
	"net/http"
)

// DefaultMaxRequestBytes matches Anthropic's documented /v1/messages limit (32 MB).
// OpenAI/Gemini limits are similar or higher; we standardize on this so relay
// never rejects a body the upstream would accept. Override with RELAY_MAX_REQUEST_BYTES.
const DefaultMaxRequestBytes int64 = 32 * 1024 * 1024

func LimitBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

func IsBodyTooLargeError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

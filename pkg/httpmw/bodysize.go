package httpmw

import (
	"errors"
	"net/http"
)

const DefaultMaxRequestBytes int64 = 2 * 1024 * 1024

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

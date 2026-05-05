package httpmw

import (
	"errors"
	"net/http"
	"os"
	"strconv"
)

const DefaultMaxRequestBytes int64 = 2 * 1024 * 1024

func MaxRequestBytesFromEnv() int64 {
	s := os.Getenv("RELAY_MAX_REQUEST_BYTES")
	if s == "" {
		return DefaultMaxRequestBytes
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return DefaultMaxRequestBytes
	}
	return n
}

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

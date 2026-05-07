// Package auth provides caller bearer-token middleware for Relay.
// Constant-time key comparison is used (crypto/subtle); the middleware is not
// unit-tested for that property because ConstantTimeCompare is well-tested in
// the standard library.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

// Rejection reason labels for relay_auth_rejected_total.
const (
	ReasonMissing = "missing"
	ReasonInvalid = "invalid"
)

var (
	rejectedMissing atomic.Uint64
	rejectedInvalid atomic.Uint64
)

// AuthRejected returns the cumulative count for the given reason label.
// Valid reason values are ReasonMissing and ReasonInvalid.
func AuthRejected(reason string) uint64 {
	switch reason {
	case ReasonMissing:
		return rejectedMissing.Load()
	case ReasonInvalid:
		return rejectedInvalid.Load()
	}
	return 0
}

func incRejected(reason string) {
	switch reason {
	case ReasonMissing:
		rejectedMissing.Add(1)
	case ReasonInvalid:
		rejectedInvalid.Add(1)
	}
}

var deny401 = mustMarshal(map[string]any{
	"error": map[string]string{
		"type":    "invalid_request_error",
		"code":    "missing_authorization",
		"message": "Missing or invalid API key.",
	},
})

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func reject(w http.ResponseWriter, reason string) {
	incRejected(reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(deny401)
}

// Middleware returns a chi-compatible middleware that enforces bearer-token auth.
// When keys is nil or empty the middleware is a passthrough (fail-open); the
// boot WARN is the operator's signal, not per-request noise.
func Middleware(keys [][]byte) func(http.Handler) http.Handler {
	if len(keys) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("Authorization")
			if raw == "" {
				reject(w, ReasonMissing)
				return
			}
			if !strings.HasPrefix(raw, "Bearer ") {
				reject(w, ReasonInvalid)
				return
			}
			token := raw[len("Bearer "):]
			if token == "" {
				reject(w, ReasonInvalid)
				return
			}
			tok := []byte(token)
			for _, k := range keys {
				if subtle.ConstantTimeCompare(tok, k) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			reject(w, ReasonInvalid)
		})
	}
}

// ParseKeys parses one or more env var values (not names) into a set of key
// bytes ready for ConstantTimeCompare. Values are split on commas and newlines;
// whitespace is trimmed; empty entries are dropped. The caller typically passes
// os.Getenv("RELAY_API_KEY") and os.Getenv("RELAY_API_KEYS").
func ParseKeys(env ...string) [][]byte {
	var out [][]byte
	for _, e := range env {
		for _, seg := range strings.FieldsFunc(e, func(r rune) bool {
			return r == ',' || r == '\n'
		}) {
			seg = strings.TrimSpace(seg)
			if seg != "" {
				out = append(out, []byte(seg))
			}
		}
	}
	return out
}

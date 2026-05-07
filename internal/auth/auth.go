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
)

// Rejection reason labels for relay_auth_rejected_total.
const (
	ReasonMissing = "missing"
	ReasonInvalid = "invalid"
)

func incRejected(reason string) {
	switch reason {
	case ReasonMissing:
		metricRejectedMissing.Inc()
	case ReasonInvalid:
		metricRejectedInvalid.Inc()
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
			var token string

			// Accept both "Authorization: Bearer <token>" (OpenAI convention) and
			// "x-api-key: <token>" (Anthropic convention) so that Anthropic-SDK
			// clients (e.g. Claude Code with ANTHROPIC_BASE_URL) can reach Relay
			// without reconfiguring their auth header.
			if raw := r.Header.Get("Authorization"); raw != "" {
				if !strings.HasPrefix(raw, "Bearer ") {
					reject(w, ReasonInvalid)
					return
				}
				token = raw[len("Bearer "):]
				if token == "" {
					// "Bearer " with no value is treated as an invalid credential,
					// not a missing one — matches original behaviour.
					reject(w, ReasonInvalid)
					return
				}
			} else if xak := r.Header.Get("x-api-key"); xak != "" {
				token = xak
			}

			if token == "" {
				reject(w, ReasonMissing)
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

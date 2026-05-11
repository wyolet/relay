// Package auth provides caller bearer-token middleware for Relay.
// Constant-time key comparison is used (crypto/subtle); the middleware is not
// unit-tested for that property because ConstantTimeCompare is well-tested in
// the standard library.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Subject identifies the authenticated caller. Populated by the bearer-auth
// middleware and stashed on the request context for downstream consumers
// (routing uses PolicyRef to override the provider's defaultPolicy).
type Subject struct {
	// KeyName is the catalog name of the matched RelayKey. Empty when the
	// request authenticated via a legacy env-bound key.
	KeyName string
	// PolicyRef, when non-empty, names the Policy that should apply to this
	// request, overriding the provider's defaultPolicy.
	PolicyRef string
}

type subjectCtxKey struct{}

// WithSubject returns a context with subj attached.
func WithSubject(ctx context.Context, subj Subject) context.Context {
	return context.WithValue(ctx, subjectCtxKey{}, subj)
}

// SubjectFrom extracts the auth subject from ctx. Zero value when absent.
func SubjectFrom(ctx context.Context) Subject {
	if v, ok := ctx.Value(subjectCtxKey{}).(Subject); ok {
		return v
	}
	return Subject{}
}

// Lookup resolves a bearer token to an auth Subject. Returns (Subject, true)
// when the token is valid and the key is enabled and not revoked. The catalog
// snapshot is the expected backing store; lookups must not touch Postgres.
type Lookup func(token string) (Subject, bool)

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

// Middleware returns a chi-compatible middleware that enforces bearer-token
// auth. envKeys is the legacy env-bound key list; lookup, when non-nil,
// resolves bearer tokens against the catalog (RelayKey snapshot). When both
// envKeys and lookup are empty/nil the middleware is a passthrough (fail-open);
// the boot WARN is the operator's signal, not per-request noise.
//
// On a successful catalog match the resolved Subject (with PolicyRef) is
// stashed on the request context via WithSubject so routing can honour it.
func Middleware(envKeys [][]byte, lookup Lookup) func(http.Handler) http.Handler {
	if len(envKeys) == 0 && lookup == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractBearer(r, w)
			if !ok {
				return
			}

			// Catalog lookup wins when present — that's the managed key path.
			if lookup != nil {
				if subj, ok := lookup(token); ok {
					next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), subj)))
					return
				}
			}

			// Env-bound fallback for bootstrap deployments.
			tok := []byte(token)
			for _, k := range envKeys {
				if subtle.ConstantTimeCompare(tok, k) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			reject(w, ReasonInvalid)
		})
	}
}

// extractBearer pulls the bearer token from the request. Writes a 401 and
// returns ok=false when the credential is malformed or missing.
func extractBearer(r *http.Request, w http.ResponseWriter) (string, bool) {
	var token string
	// Header priority:
	//  1. X-WR-API-Key — lets clients that use Authorization for their own
	//     upstream auth (e.g. Claude Code OAuth Bearer) send the Relay
	//     customer key out-of-band.
	//  2. Authorization: Bearer <token> — OpenAI SDK convention.
	//  3. x-api-key — Anthropic SDK convention.
	if xrk := r.Header.Get("X-WR-API-Key"); xrk != "" {
		token = xrk
	} else if raw := r.Header.Get("Authorization"); raw != "" {
		if !strings.HasPrefix(raw, "Bearer ") {
			reject(w, ReasonInvalid)
			return "", false
		}
		token = raw[len("Bearer "):]
		if token == "" {
			reject(w, ReasonInvalid)
			return "", false
		}
	} else if xak := r.Header.Get("x-api-key"); xak != "" {
		token = xak
	}
	if token == "" {
		reject(w, ReasonMissing)
		return "", false
	}
	return token, true
}

// HashToken returns the lowercase sha256 hex of token, matching the
// catalog.RelayKey.Spec.KeyHash format.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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

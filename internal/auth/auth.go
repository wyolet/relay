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
// (routing uses PolicyRef to override the provider's defaultPolicy; the
// pipeline uses PassthroughAuth to forward the upstream credential).
type Subject struct {
	// KeyName is the catalog name of the matched RelayKey. Empty when the
	// request authenticated via a legacy env-bound key or anonymously via
	// the global passthrough config.
	KeyName string
	// PolicyRef, when non-empty, names the Policy that should apply to this
	// request, overriding the provider's defaultPolicy.
	PolicyRef string
	// PassthroughAuth, when non-empty, is the raw inbound Authorization
	// header value to forward verbatim to upstream (BYO-credential flow).
	// Includes the "Bearer " prefix so the pipeline can pass it through
	// without reconstruction.
	PassthroughAuth string
	// Anonymous reports whether the caller had no Relay key (matched the
	// global Passthrough.unauthenticated.enabled gate). Always implies
	// PassthroughAuth is set.
	Anonymous bool
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

// LookupResult is what Lookup returns when a bearer matches a managed key.
// PassthroughAllowed is the per-key opt-in for BYO-credential forwarding;
// the middleware combines it with the global PassthroughGate to decide
// whether to populate Subject.PassthroughAuth.
type LookupResult struct {
	KeyName            string
	PolicyRef          string
	PassthroughAllowed bool
}

// Lookup resolves a bearer token to a managed-key descriptor. Returns
// (result, true) when the token is valid, enabled, and not revoked. The
// catalog snapshot is the expected backing store; lookups must not touch PG.
type Lookup func(token string) (LookupResult, bool)

// PassthroughGate reports the global passthrough configuration. The middleware
// reads it on every request to decide whether BYO-credential traffic is
// accepted at all (Enabled) and whether anonymous BYO is accepted
// (UnauthenticatedEnabled).
type PassthroughGate struct {
	Enabled                bool
	UnauthenticatedEnabled bool
}

// PassthroughGateFunc returns the current PassthroughGate. Callers compose
// from catalog.Store.Passthrough() in main.
type PassthroughGateFunc func() PassthroughGate

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

// Middleware enforces bearer-token auth and resolves the passthrough story
// for each request. envKeys is the legacy env-bound key list; lookup resolves
// managed RelayKeys; ptGate reports the global Passthrough config (when nil,
// passthrough is treated as disabled).
//
// Header priority for relay-key matching:
//  1. X-WR-API-Key — out-of-band relay key, leaves Authorization free for
//     upstream forwarding (e.g. Claude Code OAuth Bearer flows).
//  2. x-api-key — Anthropic SDK convention.
//  3. Authorization: Bearer <token> — OpenAI SDK convention; consumed last
//     so we don't accidentally forward the relay key as upstream auth.
//
// When a relay key matches via headers (1) or (2) and the request also carries
// an Authorization header, that Authorization is treated as a BYO-credential
// candidate and stamped on Subject.PassthroughAuth iff the per-key
// PassthroughAllowed AND ptGate.Enabled both hold.
//
// When no relay key matches, the request is admitted as anonymous passthrough
// iff Authorization is present AND ptGate.Enabled AND ptGate.UnauthenticatedEnabled.
//
// When envKeys, lookup, and ptGate are all empty/nil the middleware is a
// passthrough no-op (fail-open); the boot WARN is the operator's signal.
func Middleware(envKeys [][]byte, lookup Lookup, ptGate PassthroughGateFunc) func(http.Handler) http.Handler {
	if len(envKeys) == 0 && lookup == nil && ptGate == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			xrk := r.Header.Get("X-WR-API-Key")
			xak := r.Header.Get("x-api-key")
			rawAuthz := r.Header.Get("Authorization")
			authzToken := bearerToken(rawAuthz)

			// Try managed/env match in priority order: xrk → xak → authzToken.
			candidates := []struct {
				token string
				kind  candidateKind
			}{
				{xrk, candidateXRK},
				{xak, candidateXAK},
				{authzToken, candidateAuthz},
			}
			var subj Subject
			var matched bool
			var consumedKind candidateKind
			for _, c := range candidates {
				if c.token == "" {
					continue
				}
				if lookup != nil {
					if res, ok := lookup(c.token); ok {
						subj = Subject{KeyName: res.KeyName, PolicyRef: res.PolicyRef}
						matched = true
						consumedKind = c.kind
						// Stamp passthrough auth when the relay key matched
						// via xrk/xak (leaving Authorization as upstream
						// candidate) AND per-key + global gates allow.
						if c.kind != candidateAuthz && rawAuthz != "" && res.PassthroughAllowed {
							if g := gate(ptGate); g.Enabled {
								subj.PassthroughAuth = rawAuthz
							}
						}
						break
					}
				}
				if matchesEnv(c.token, envKeys) {
					matched = true
					consumedKind = c.kind
					break
				}
			}

			if matched {
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), subj)))
				return
			}

			// No relay key match. Try anonymous passthrough.
			if rawAuthz != "" {
				g := gate(ptGate)
				if g.Enabled && g.UnauthenticatedEnabled {
					anonSubj := Subject{PassthroughAuth: rawAuthz, Anonymous: true}
					next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), anonSubj)))
					return
				}
			}

			// Reject with the appropriate reason.
			if xrk == "" && xak == "" && authzToken == "" && rawAuthz == "" {
				reject(w, ReasonMissing)
				return
			}
			_ = consumedKind
			reject(w, ReasonInvalid)
		})
	}
}

type candidateKind int

const (
	candidateXRK candidateKind = iota
	candidateXAK
	candidateAuthz
)

// bearerToken strips the "Bearer " prefix from an Authorization header value.
// Returns "" when the header is empty or doesn't use the Bearer scheme — the
// caller decides whether that is a hard error.
func bearerToken(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "Bearer ") {
		return ""
	}
	return raw[len("Bearer "):]
}

func matchesEnv(token string, envKeys [][]byte) bool {
	tok := []byte(token)
	for _, k := range envKeys {
		if subtle.ConstantTimeCompare(tok, k) == 1 {
			return true
		}
	}
	return false
}

func gate(fn PassthroughGateFunc) PassthroughGate {
	if fn == nil {
		return PassthroughGate{}
	}
	return fn()
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

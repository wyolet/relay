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

	"github.com/wyolet/relay/internal/transport/mode"
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
	// Mode is the resolved proxy mode for this request. Set by Middleware
	// when X-WR-Proxy-Mode header is present; ModeNormal for default requests.
	Mode mode.Mode
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

var deny400ProxyMode = mustMarshal(map[string]any{
	"error": map[string]string{
		"type":    "invalid_request_error",
		"code":    "invalid_proxy_mode",
		"message": "Invalid X-WR-Proxy-Mode header value.",
	},
})

var deny400MissingProviderKey = mustMarshal(map[string]any{
	"error": map[string]string{
		"type":    "invalid_request_error",
		"code":    "missing_provider_key",
		"message": "Proxy mode requires Authorization: Bearer <provider-key>.",
	},
})

var deny401 = mustMarshal(map[string]any{
	"error": map[string]string{
		"type":    "invalid_request_error",
		"code":    "missing_authorization",
		"message": "Missing or invalid API key.",
	},
})

var deny403Passthrough = mustMarshal(map[string]any{
	"error": map[string]string{
		"type":    "invalid_request_error",
		"code":    "passthrough_not_allowed",
		"message": "Proxy mode is not permitted for this key.",
	},
})

var deny401Anonymous = mustMarshal(map[string]any{
	"error": map[string]string{
		"type":    "invalid_request_error",
		"code":    "unauthenticated_proxy_disabled",
		"message": "Anonymous proxy mode is not enabled.",
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

func rejectWith(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Middleware enforces bearer-token auth and resolves the passthrough story
// for each request. envKeys is the legacy env-bound key list; lookup resolves
// managed RelayKeys; ptGate reports the global Passthrough config (when nil,
// passthrough is treated as disabled).
//
// Mode determination is delegated to internal/transport/mode.Classify:
//   - ModeNormal (default / "No-Proxy"): Relay key required via X-WR-API-Key,
//     x-api-key, or Authorization Bearer. 401 if missing or invalid.
//   - ModeProxyAuthed: Relay key via X-WR-API-Key required; the key's
//     PassthroughAllowed flag must be true. 401/403 otherwise. The provider
//     key from Authorization is stamped on Subject.PassthroughAuth.
//   - ModeProxyAnonymous: no Relay key; global gate.UnauthenticatedEnabled
//     must be on. 401 if off. The provider key from Authorization is stamped.
//   - Invalid X-WR-Proxy-Mode value or missing provider key in Proxy mode → 400.
//
// When envKeys, lookup, and ptGate are all empty/nil the middleware is a
// passthrough no-op (fail-open); the boot WARN is the operator's signal.
func Middleware(envKeys [][]byte, lookup Lookup, ptGate PassthroughGateFunc) func(http.Handler) http.Handler {
	if len(envKeys) == 0 && lookup == nil && ptGate == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls, err := mode.Classify(r)
			if err != nil {
				switch err {
				case mode.ErrMissingProviderKey:
					rejectWith(w, http.StatusBadRequest, deny400MissingProviderKey)
				default:
					rejectWith(w, http.StatusBadRequest, deny400ProxyMode)
				}
				return
			}

			switch cls.Mode {
			case mode.ModeNormal:
				handleNormal(w, r, cls, envKeys, lookup, ptGate, next)
			case mode.ModeProxyAuthed:
				handleProxyAuthed(w, r, cls, lookup, ptGate, next)
			case mode.ModeProxyAnonymous:
				handleProxyAnonymous(w, r, cls, ptGate, next)
			default:
				reject(w, ReasonMissing)
			}
		})
	}
}

// handleNormal processes ModeNormal requests.
func handleNormal(
	w http.ResponseWriter, r *http.Request,
	cls mode.Classification,
	envKeys [][]byte, lookup Lookup, ptGate PassthroughGateFunc,
	next http.Handler,
) {
	relayKey := cls.RelayKey
	rawAuthz := r.Header.Get("Authorization")

	// Determine which header slot provided the relay key for passthrough logic.
	relayKeyViaAuthz := relayKey != "" && relayKey == bearerToken(rawAuthz)

	if relayKey != "" {
		if lookup != nil {
			if res, ok := lookup(relayKey); ok {
				subj := Subject{KeyName: res.KeyName, PolicyRef: res.PolicyRef, Mode: cls.Mode}
				// Passthrough: relay key not via Authorization AND Authorization present.
				if !relayKeyViaAuthz && rawAuthz != "" && res.PassthroughAllowed {
					if g := gate(ptGate); g.Enabled {
						subj.PassthroughAuth = rawAuthz
					}
				}
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), subj)))
				return
			}
		}
		if matchesEnv(relayKey, envKeys) {
			next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), Subject{Mode: cls.Mode})))
			return
		}
		reject(w, ReasonInvalid)
		return
	}

	// No relay key — try anonymous passthrough (only in normal mode when gate allows).
	if rawAuthz != "" {
		g := gate(ptGate)
		if g.Enabled && g.UnauthenticatedEnabled {
			anonSubj := Subject{PassthroughAuth: rawAuthz, Anonymous: true, Mode: cls.Mode}
			next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), anonSubj)))
			return
		}
	}

	if rawAuthz == "" && relayKey == "" {
		reject(w, ReasonMissing)
	} else {
		reject(w, ReasonInvalid)
	}
}

// handleProxyAuthed processes ModeProxyAuthed requests (relay key + provider key).
func handleProxyAuthed(
	w http.ResponseWriter, r *http.Request,
	cls mode.Classification,
	lookup Lookup, ptGate PassthroughGateFunc,
	next http.Handler,
) {
	if cls.RelayKey == "" {
		reject(w, ReasonMissing)
		return
	}
	if lookup == nil {
		// No lookup configured — can't verify passthrough permission.
		reject(w, ReasonInvalid)
		return
	}
	res, ok := lookup(cls.RelayKey)
	if !ok {
		reject(w, ReasonInvalid)
		return
	}
	if !res.PassthroughAllowed {
		rejectWith(w, http.StatusForbidden, deny403Passthrough)
		return
	}
	g := gate(ptGate)
	if !g.Enabled {
		rejectWith(w, http.StatusForbidden, deny403Passthrough)
		return
	}
	// Provider key is in Authorization; stamp it as PassthroughAuth (with "Bearer " prefix).
	subj := Subject{
		KeyName:         res.KeyName,
		PolicyRef:       res.PolicyRef,
		PassthroughAuth: "Bearer " + cls.ProviderKey,
		Mode:            cls.Mode,
	}
	next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), subj)))
}

// handleProxyAnonymous processes ModeProxyAnonymous requests (no relay key).
func handleProxyAnonymous(
	w http.ResponseWriter, r *http.Request,
	cls mode.Classification,
	ptGate PassthroughGateFunc,
	next http.Handler,
) {
	g := gate(ptGate)
	if !g.Enabled || !g.UnauthenticatedEnabled {
		rejectWith(w, http.StatusUnauthorized, deny401Anonymous)
		return
	}
	subj := Subject{
		PassthroughAuth: "Bearer " + cls.ProviderKey,
		Anonymous:       true,
		Mode:            cls.Mode,
	}
	next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), subj)))
}

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

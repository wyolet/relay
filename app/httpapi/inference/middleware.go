package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/relaykey"
)

// ctxRelayKeyT is the context-value key used to stash the authenticated
// RelayKey for handlers to read via RelayKeyFromContext.
type ctxRelayKeyT struct{}

// RelayKeyFromContext returns the authenticated relay key from ctx, or
// nil if no relay-key middleware fired.
func RelayKeyFromContext(ctx context.Context) *relaykey.RelayKey {
	if v, ok := ctx.Value(ctxRelayKeyT{}).(*relaykey.RelayKey); ok {
		return v
	}
	return nil
}

// RelayKeyAuthMiddleware authenticates Authorization: Bearer <token>
// against the snapshot's RelayKey-by-hash index. On success it injects
// the *relaykey.RelayKey into ctx; on failure 401s with the OpenAI-
// shape error envelope.
//
// cat is read on every request so admins toggling Enabled / RevokedAt
// take effect within the NOTIFY debounce window.
func RelayKeyAuthMiddleware(cat *appcatalog.Catalog) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := bearer(r.Header.Get("Authorization"))
			if tok == "" {
				writeAuthErr(w, "missing bearer token")
				return
			}
			rk, ok := cat.Current().RelayKeyByHash(hashToken(tok))
			if !ok {
				writeAuthErr(w, "invalid api key")
				return
			}
			if rk.Spec.Enabled != nil && !*rk.Spec.Enabled {
				writeAuthErr(w, "api key disabled")
				return
			}
			if rk.Spec.RevokedAt != nil {
				writeAuthErr(w, "api key revoked")
				return
			}
			ctx := context.WithValue(r.Context(), ctxRelayKeyT{}, rk)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return h[len(prefix):]
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeAuthErr(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"unauthenticated","message":"` + msg + `"}}`))
}

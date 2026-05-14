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

// RelayKeyAuthMiddleware authenticates the inbound relay key according
// to the request's Mode classification (set by ClassifyMiddleware
// upstream):
//
//   - ModeNormal       — relay key is required; lookup must succeed.
//   - ModeProxyAuthed  — relay key is required; lookup must succeed.
//   - ModeProxyAnonymous — no relay key; this middleware is a no-op and
//     no *RelayKey is stashed on ctx.
//
// Snapshot is read on every request so admins toggling Enabled /
// RevokedAt take effect within the NOTIFY debounce window.
func RelayKeyAuthMiddleware(cat *appcatalog.Catalog) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls := ClassificationFrom(r.Context())
			if cls.Mode == ModeProxyAnonymous {
				// Gate (Settings.ProxyMode.AllowUnauthenticated) is checked
				// downstream in the handler; this middleware just doesn't
				// require a relay key.
				next.ServeHTTP(w, r)
				return
			}
			if cls.RelayKey == "" {
				writeAuthErr(w, "missing relay key")
				return
			}
			rk, ok := cat.Current().RelayKeyByHash(hashToken(cls.RelayKey))
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

package inference

import (
	"context"
	"net/http"
	"strings"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
)

// ctxKeyT is the context-value key used to stash the authenticated
// Key for handlers to read via KeyFromContext.
type ctxKeyT struct{}

// ctxPrincipalT is the context-value key for the resolved Principal.
type ctxPrincipalT struct{}

// ctxSnapshotT is the context-value key for the snapshot the credential was
// resolved against.
type ctxSnapshotT struct{}

// WithSnapshot pins snap as the catalog view for everything downstream of
// ctx. The credential middleware and the WebSocket per-frame path are the
// only writers.
func WithSnapshot(ctx context.Context, snap *appcatalog.Snapshot) context.Context {
	return context.WithValue(ctx, ctxSnapshotT{}, snap)
}

// SnapshotFrom returns the catalog view this request was authenticated
// against, or nil when no credential middleware ran (anonymous proxy).
// Downstream phases read it rather than cat.Current() so a reload landing
// mid-request cannot split one request across two snapshots.
func SnapshotFrom(ctx context.Context) *appcatalog.Snapshot {
	if v, ok := ctx.Value(ctxSnapshotT{}).(*appcatalog.Snapshot); ok {
		return v
	}
	return nil
}

// reserveIdentity returns what the inbound reservation is scoped by: the
// caller's team (the kv hash tag) and, for a token, the jti whose denylist
// entry rides the same script.
func reserveIdentity(ctx context.Context) (teamID, tokenJTI string) {
	p := PrincipalFrom(ctx)
	if p == nil {
		return "", ""
	}
	if p.CredentialKind == CredentialToken {
		return p.TeamID, p.CredentialID
	}
	return p.TeamID, ""
}

// KeyFromContext returns the authenticated relay key from ctx, or
// nil if no key middleware fired.
func KeyFromContext(ctx context.Context) *key.Key {
	if v, ok := ctx.Value(ctxKeyT{}).(*key.Key); ok {
		return v
	}
	return nil
}

// PrincipalFrom returns the resolved principal from ctx, or nil when the
// request carried no credential (anonymous proxy mode).
func PrincipalFrom(ctx context.Context) *Principal {
	if v, ok := ctx.Value(ctxPrincipalT{}).(*Principal); ok {
		return v
	}
	return nil
}

// PrincipalMiddleware authenticates the inbound credential according to the
// request's Mode classification (set by ClassifyMiddleware upstream) and
// resolves who it acts as:
//
//   - ModeNormal       — credential is required; lookup must succeed.
//   - ModeProxyAuthed  — credential is required; lookup must succeed and the
//     principal must be allowed to bring its own upstream key.
//   - ModeProxyAnonymous — no credential; this middleware is a no-op and
//     neither a *Key nor a *Principal is stashed on ctx.
//
// The bearer is either a key or a relay-minted token, told apart by shape
// (looksLikeToken); both resolve to the same Principal and only the lookup
// differs. Every read is against the in-memory snapshot, re-read per request
// so admin edits take effect within the NOTIFY debounce window.
func PrincipalMiddleware(cat *appcatalog.Catalog, tokens *TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls := ClassificationFrom(r.Context())
			if cls.Mode == ModeProxyAnonymous {
				// Gate (Settings.ProxyMode.AllowUnauthenticated) is checked
				// downstream in the handler; this middleware just doesn't
				// require a key.
				next.ServeHTTP(w, r)
				return
			}
			if cls.Key == "" {
				writeAuthErr(w, "missing relay key")
				return
			}
			snap := cat.Current()
			p, k, ok := authenticate(w, snap, tokens, cls.Key)
			if !ok {
				return
			}
			if !authorizePrincipal(w, snap, p, cls.Mode) {
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), snap, p, k)))
		})
	}
}

// AuthenticateMiddleware resolves who the caller is exactly as PrincipalMiddleware does, but resolves no policy: for endpoints that record what a caller reports and route nothing, where a credential without a policy is still a valid reporter. A credential is always required.
func AuthenticateMiddleware(cat *appcatalog.Catalog, tokens *TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls := ClassificationFrom(r.Context())
			if cls.Key == "" {
				writeAuthErr(w, "missing relay key")
				return
			}
			snap := cat.Current()
			p, k, ok := authenticate(w, snap, tokens, cls.Key)
			if !ok {
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), snap, p, k)))
		})
	}
}

// authenticate resolves a bearer, a key or a relay-minted token, to its principal. Reports false after writing the 401.
func authenticate(w http.ResponseWriter, snap *appcatalog.Snapshot, tokens *TokenVerifier, bearer string) (*Principal, *key.Key, bool) {
	if looksLikeToken(bearer) {
		p, ok := tokenPrincipal(w, snap, tokens, bearer)
		return p, nil, ok
	}
	return keyPrincipal(w, snap, bearer)
}

// withPrincipal stashes the resolved principal, its key when it has one, and the snapshot it resolved against.
func withPrincipal(ctx context.Context, snap *appcatalog.Snapshot, p *Principal, k *key.Key) context.Context {
	ctx = WithSnapshot(ctx, snap)
	if k != nil {
		ctx = context.WithValue(ctx, ctxKeyT{}, k)
	}
	return context.WithValue(ctx, ctxPrincipalT{}, p)
}

// authorizePrincipal resolves p's policy and applies the mode's credential gate. Shared by the HTTP edge and each WebSocket frame. Reports false after writing the response.
func authorizePrincipal(w http.ResponseWriter, snap *appcatalog.Snapshot, p *Principal, mode Mode) bool {
	if !resolvePolicy(w, snap, p) {
		return false
	}
	if mode == ModeProxyAuthed && !p.PassthroughAllowed {
		writeForbidden(w, "passthrough_forbidden", "this credential may not forward upstream keys")
		return false
	}
	return true
}

func bearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return h[len(prefix):]
}

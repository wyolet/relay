package inference

import (
	"context"
	"net/http"
	"strings"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/revocation"
)

// requestState is the one context value the middleware chain gives a request: ClassifyMiddleware sets it and the credential middleware fills it in, so the chain adds no further context layers or request copies.
type requestState struct {
	cls       Classification
	key       *key.Key
	principal *Principal
	snap      *appcatalog.Snapshot
	// snapSet tells a pinned nil snapshot from none.
	snapSet bool
}

type ctxStateT struct{}

func stateFrom(ctx context.Context) *requestState {
	s, _ := ctx.Value(ctxStateT{}).(*requestState)
	return s
}

// derivedState returns a copy of ctx's nearest state, or a new one, for the caller to change and put on a child context. A state already on a context may be read by other goroutines, so only the credential middleware changes one in place (fillPrincipal).
func derivedState(ctx context.Context) *requestState {
	s := new(requestState)
	if outer := stateFrom(ctx); outer != nil {
		*s = *outer
	}
	return s
}

func withState(ctx context.Context, s *requestState) context.Context {
	return context.WithValue(ctx, ctxStateT{}, s)
}

// The keys below are what contexts built without the middleware chain (tests) carry; the accessors fall back to them where the state leaves a value unset.

// ctxKeyT is the context-value key used to stash the authenticated
// Key for handlers to read via KeyFromContext.
type ctxKeyT struct{}

// ctxPrincipalT is the context-value key for the resolved Principal.
type ctxPrincipalT struct{}

// ctxSnapshotT is the context-value key for the snapshot the credential was
// resolved against.
type ctxSnapshotT struct{}

// WithSnapshot pins snap as the catalog view for everything downstream of
// ctx, for callers outside the middleware chain; the chain and the WebSocket
// per-frame path set it on the request state directly.
func WithSnapshot(ctx context.Context, snap *appcatalog.Snapshot) context.Context {
	s := derivedState(ctx)
	s.snap, s.snapSet = snap, true
	return withState(ctx, s)
}

// SnapshotFrom returns the catalog view this request was authenticated
// against, or nil when no credential middleware ran (anonymous proxy).
// Downstream phases read it rather than cat.Current() so a reload landing
// mid-request cannot split one request across two snapshots.
func SnapshotFrom(ctx context.Context) *appcatalog.Snapshot {
	if s := stateFrom(ctx); s != nil && s.snapSet {
		return s.snap
	}
	if v, ok := ctx.Value(ctxSnapshotT{}).(*appcatalog.Snapshot); ok {
		return v
	}
	return nil
}

// KeyFromContext returns the authenticated relay key from ctx, or
// nil if no key middleware fired.
func KeyFromContext(ctx context.Context) *key.Key {
	if s := stateFrom(ctx); s != nil && s.key != nil {
		return s.key
	}
	if v, ok := ctx.Value(ctxKeyT{}).(*key.Key); ok {
		return v
	}
	return nil
}

// PrincipalFrom returns the resolved principal from ctx, or nil when the
// request carried no credential (anonymous proxy mode).
func PrincipalFrom(ctx context.Context) *Principal {
	if s := stateFrom(ctx); s != nil && s.principal != nil {
		return s.principal
	}
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
//   - ModeProxyAnonymous — no credential; neither a *Key nor a *Principal is
//     stashed on ctx.
//
// Both proxy modes are also refused here when the proxy-mode setting does not admit them, so a refused request never has its body read, takes an admission slot or leaves a usage record.
//
// The bearer is either a key or a relay-minted token, told apart by shape
// (looksLikeToken); both resolve to the same Principal and only the lookup
// differs. Every read is against the published plans and the in-memory
// revoked-token set, re-read per request so admin edits take effect within
// the NOTIFY debounce window.
func PrincipalMiddleware(a *Auth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls := ClassificationFrom(r.Context())
			if cls.Mode == ModeProxyAnonymous {
				if authorizeProxyMode(w, a.Catalog, cls.Mode) {
					next.ServeHTTP(w, r)
				}
				return
			}
			if cls.Key == "" {
				writeAuthErr(w, "missing relay key")
				return
			}
			p, k, ok := a.authenticate(w, cls.Key)
			if !ok {
				return
			}
			if !authorizePrincipal(w, p, cls.Mode) {
				return
			}
			if !authorizeProxyMode(w, a.Catalog, cls.Mode) {
				return
			}
			next.ServeHTTP(w, fillPrincipal(r, a.Catalog.Current(), p, k))
		})
	}
}

// AuthenticateMiddleware resolves who the caller is exactly as PrincipalMiddleware does, but refuses nothing its caller ladder would: for endpoints that record what a caller reports and route nothing, where any valid credential is a valid reporter. A credential is always required.
func AuthenticateMiddleware(a *Auth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls := ClassificationFrom(r.Context())
			if cls.Key == "" {
				writeAuthErr(w, "missing relay key")
				return
			}
			p, k, ok := a.authenticate(w, cls.Key)
			if !ok {
				return
			}
			next.ServeHTTP(w, fillPrincipal(r, a.Catalog.Current(), p, k))
		})
	}
}

// Auth is what the credential middleware reads: the published plans, the revoked-token set, the token verifier, and the catalog for the proxy-mode setting.
type Auth struct {
	Catalog *appcatalog.Catalog
	Plans   PlanSource
	Revoked *revocation.Set
	Tokens  *TokenVerifier
}

// PlanSource is where the request path loads the published plans; *plan.Holder implements it.
type PlanSource interface{ Load() *plan.Root }

// authenticate resolves a bearer, a key or a relay-minted token, to its principal. Reports false after writing the response: 401 for a bad credential, 503 before the first plans are published.
func (a *Auth) authenticate(w http.ResponseWriter, bearer string) (*Principal, *key.Key, bool) {
	root := a.root()
	if root == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "not_ready", "relay is starting; retry shortly")
		return nil, nil, false
	}
	if looksLikeToken(bearer) {
		p, ok := tokenPrincipal(w, a, root, bearer)
		return p, nil, ok
	}
	return keyPrincipal(w, root, bearer)
}

func (a *Auth) root() *plan.Root {
	if a == nil || a.Plans == nil {
		return nil
	}
	return a.Plans.Load()
}

// fillPrincipal records the resolved principal, its key when it has one, and the snapshot it resolved against on r's state.
func fillPrincipal(r *http.Request, snap *appcatalog.Snapshot, p *Principal, k *key.Key) *http.Request {
	// Filled in place: one goroutine runs the middleware chain, and nothing past this middleware has seen the state yet. A WebSocket connection's frames share the upgrade's state, so they each take a copy (frameRequest).
	if s := stateFrom(r.Context()); s != nil {
		s.setPrincipal(snap, p, k)
		return r
	}
	return r.WithContext(withPrincipal(r.Context(), snap, p, k))
}

// withPrincipal is fillPrincipal on a copy of ctx's state.
func withPrincipal(ctx context.Context, snap *appcatalog.Snapshot, p *Principal, k *key.Key) context.Context {
	s := derivedState(ctx)
	s.setPrincipal(snap, p, k)
	return withState(ctx, s)
}

func (s *requestState) setPrincipal(snap *appcatalog.Snapshot, p *Principal, k *key.Key) {
	s.snap, s.snapSet, s.principal = snap, true, p
	if k != nil {
		s.key = k
	}
}

// authorizePrincipal answers a caller ladder that fails closed and applies the mode's credential gate. Shared by the HTTP edge and each WebSocket frame. Reports false after writing the response.
func authorizePrincipal(w http.ResponseWriter, p *Principal, mode Mode) bool {
	if !admitCaller(w, p.Caller) {
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

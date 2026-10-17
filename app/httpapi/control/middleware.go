package control

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/auth/authn"
	"github.com/wyolet/relay/auth/principal"
)

// authenticate runs the control plane's authenticator chain and hands handlers its principal as an Actor. The admin token comes first, so it outranks a session the same request carries; a request neither claims continues with no Actor and the protected routes refuse it. A session of a disabled account is refused on every route but logout.
func authenticate(d Deps) func(http.Handler) http.Handler {
	chain := authn.Chain{
		Authenticators: []authn.Authenticator{adminTokenAuth(d.AdminToken)},
		ResolveUser:    resolveAccountStatus(d.Catalog),
		Admit:          authn.AdmitActive,
	}
	if d.Sessions != nil {
		chain.Authenticators = append(chain.Authenticators, d.Sessions)
	}
	return func(next http.Handler) http.Handler {
		withActor := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p := principal.From(r.Context()); p != nil {
				r = r.WithContext(actor.WithActor(r.Context(), actor.FromPrincipal(p)))
			}
			next.ServeHTTP(w, r)
		})
		return authn.Middleware(chain, func(w http.ResponseWriter, r *http.Request, err error) {
			// A disabled account must still be able to end its own session.
			if errors.Is(err, authn.ErrInactive) && isLogout(r) {
				withActor.ServeHTTP(w, r)
				return
			}
			rejectAuthentication(w, r, err)
		})(withActor)
	}
}

// resolveAccountStatus marks the admin token active, and a session active unless the snapshot holds its account as disabled. Snapshot reads only: an account disabled while its session survived (a failed session purge, a login racing the disable) is refused here within one NOTIFY. A nil catalog cannot tell, so it admits.
func resolveAccountStatus(cat *appcatalog.Catalog) func(context.Context, *principal.Principal) error {
	return func(_ context.Context, p *principal.Principal) error {
		p.Status = principal.StatusActive
		if p.Kind != principal.KindAdmin && cat != nil && cat.Current().UserDisabled(p.ID) {
			p.Status = principal.StatusDisabled
		}
		return nil
	}
}

// isLogout reports whether r is POST /auth/logout under wherever the control API is mounted, reading the path the way chi routes it.
func isLogout(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := r.URL.Path
	if r.URL.RawPath != "" {
		path = r.URL.RawPath
	}
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePath != "" {
		path = rctx.RoutePath
	}
	return path == "/auth/logout"
}

// adminTokenAuth claims a request whose bearer is RELAY_ADMIN_TOKEN. Any other request, a wrong bearer included, is left to the next authenticator. Empty disables the bypass.
type adminTokenAuth string

func (t adminTokenAuth) Authenticate(r *http.Request) (*principal.Principal, bool, error) {
	if t == "" {
		return nil, false, nil
	}
	got := bearer(r.Header.Get("Authorization"))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(t)) != 1 {
		return nil, false, nil
	}
	return &principal.Principal{
		Kind:       principal.KindAdmin,
		Username:   "admin-token",
		Admin:      true,
		Credential: principal.Credential{Kind: principal.CredentialService},
	}, true, nil
}

// rejectAuthentication answers a credential an authenticator refused, in the shape RequireActor answers a missing one. A disabled account gets the answer token mint gives it.
func rejectAuthentication(w http.ResponseWriter, _ *http.Request, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(authn.Status(err))
	if errors.Is(err, authn.ErrInactive) {
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"account is disabled"}}`))
		return
	}
	_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"` + http.StatusText(authn.Status(err)) + `"}}`))
}

// RequireActor blocks any request that has no Actor in context. Stacked
// after the authenticator chain so any credential it accepts can satisfy it.
func RequireActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !actor.From(r.Context()).IsAuthenticated() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"unauthenticated"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the value after "Bearer " in an Authorization header.
// Empty string when the header is missing or malformed.
func bearer(h string) string {
	const prefix = "Bearer "
	if len(h) < len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}

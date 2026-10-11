package control

import (
	"crypto/subtle"
	"net/http"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/auth/authn"
	"github.com/wyolet/relay/auth/principal"
)

// authenticate runs the control plane's authenticator chain and hands handlers its principal as an Actor. The admin token comes first, so it outranks a session the same request carries; a request neither claims continues with no Actor and the protected routes refuse it.
func authenticate(d Deps) func(http.Handler) http.Handler {
	chain := authn.Chain{Authenticators: []authn.Authenticator{adminTokenAuth(d.AdminToken)}}
	if d.Sessions != nil {
		chain.Authenticators = append(chain.Authenticators, d.Sessions)
	}
	withPrincipal := authn.Middleware(chain, rejectAuthentication)
	return func(next http.Handler) http.Handler {
		return withPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p := principal.From(r.Context()); p != nil {
				r = r.WithContext(actor.WithActor(r.Context(), actor.FromPrincipal(p)))
			}
			next.ServeHTTP(w, r)
		}))
	}
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

// rejectAuthentication answers a credential an authenticator refused, in the shape RequireActor answers a missing one.
func rejectAuthentication(w http.ResponseWriter, _ *http.Request, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(authn.Status(err))
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

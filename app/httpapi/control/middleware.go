package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/auth/authn"
	"github.com/wyolet/relay/auth/principal"
)

// authenticate runs the control plane's authenticator chain and hands handlers its principal as an Actor. The admin token comes first, so it outranks a session the same request carries; a request neither claims continues with no Actor and the protected routes refuse it. A session of a disabled account is ended and refused.
func authenticate(d Deps) func(http.Handler) http.Handler {
	chain := authn.Chain{
		Authenticators: []authn.Authenticator{adminTokenAuth(d.AdminToken)},
		ResolveUser:    resolveAccountStatus(d.Catalog),
		Admit:          authn.AdmitActive,
	}
	if d.Sessions != nil {
		chain.Authenticators = append(chain.Authenticators, d.Sessions)
		chain.Admit = admitOrEndSession(d.Sessions)
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

// resolveAccountStatus marks the admin token active, and a session active unless the snapshot holds its account as disabled. Snapshot reads only: an account disabled while its session survived (a failed session purge, a login racing the disable, a raw UPDATE) is caught here within one NOTIFY. A nil catalog cannot tell, so it admits.
func resolveAccountStatus(cat *appcatalog.Catalog) func(context.Context, *principal.Principal) error {
	return func(_ context.Context, p *principal.Principal) error {
		p.Status = principal.StatusActive
		if p.Kind != principal.KindAdmin && cat != nil && cat.Current().UserDisabled(p.ID) {
			p.Status = principal.StatusDisabled
		}
		return nil
	}
}

// sessionEnder is what admitOrEndSession needs from the session manager.
type sessionEnder interface {
	DestroyUser(ctx context.Context, userID string) error
	Logout(ctx context.Context) error
}

// admitOrEndSession is authn.AdmitActive that, on refusing a session, ends every session of that account and expires this request's cookie, so the browser is signed out rather than refused on every later request. A failure to end them is logged and the request is still refused.
func admitOrEndSession(sessions sessionEnder) func(context.Context, *principal.Principal) error {
	return func(ctx context.Context, p *principal.Principal) error {
		err := authn.AdmitActive(ctx, p)
		if err == nil || p.Credential.Kind != principal.CredentialSession {
			return err
		}
		if derr := sessions.DestroyUser(ctx, p.ID); derr != nil {
			slog.Error("disabled account: ending its sessions failed", "user_id", p.ID, "err", derr)
		}
		if lerr := sessions.Logout(ctx); lerr != nil {
			slog.Error("disabled account: ending this session failed", "user_id", p.ID, "err", lerr)
		}
		return err
	}
}

// accountDisabledError is the one answer every control path gives a disabled account, so a client can show one notice.
func accountDisabledError() *httpapi.APIError {
	return &httpapi.APIError{
		Err:        httpapi.APIErrorBody{Type: "authentication_error", Code: "account_disabled", Message: "account is disabled; contact your administrator"},
		HTTPStatus: http.StatusUnauthorized,
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

// rejectAuthentication answers a credential an authenticator refused, in the shape RequireActor answers a missing one. The only inactive control principal is a disabled account's session, which gets accountDisabledError.
func rejectAuthentication(w http.ResponseWriter, _ *http.Request, err error) {
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, authn.ErrInactive) {
		disabled := accountDisabledError()
		body, _ := json.Marshal(disabled)
		w.WriteHeader(disabled.HTTPStatus)
		_, _ = w.Write(body)
		return
	}
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

// OIDC login for the control plane: GET /auth/oidc/start redirects to the
// configured provider's hosted login; GET /auth/oidc/callback exchanges the
// authorization code and mints the same scs session password login mints,
// so everything downstream of login is identical for both paths.
//
// The flow is auth/oidc's relying party; this file feeds it the auth:oidc
// settings and supplies relay's user provisioning, session and audit. These
// are raw chi handlers, not huma operations: they speak browser redirects
// and cookies, not JSON.

package control

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/oidc"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/slug"
)

const oidcFlowCookie = "relay_oidc_flow"

// oidcUserStore is the narrow user-store surface the callback needs.
// directoryUsers satisfies it; tests supply a fake.
type oidcUserStore interface {
	ByUsername(ctx context.Context, username string) (*user.User, error)
	ByOIDCSubject(ctx context.Context, subject string) (*user.User, error)
	Create(ctx context.Context, u *user.User) error
	UnlinkedByExternalID(ctx context.Context, externalID string) ([]*user.User, error)
	AddFirstIdentity(ctx context.Context, userID string, id directory.Identity) error
}

// sessionLogin is the slice of session.Manager the callback needs.
type sessionLogin interface {
	LoginOIDC(ctx context.Context, userID, username, oidcSubject, idpSessionID string, groups []string, roles ...string) error
}

// oidcDeps carries the seams the relying party's hooks use, injectable for tests.
type oidcDeps struct {
	cfg      func() *settings.AuthOIDC
	scim     func() *settings.AuthSCIM
	users    oidcUserStore
	sessions sessionLogin
	rp       *oidc.RelyingParty[*user.User]
}

func newOIDCDeps(d Deps) *oidcDeps {
	users := directoryUsers{Store: d.Users, ops: newDirectoryOps(d)}
	if d.Stores != nil {
		users.clients = d.Stores.SCIMClients
	}
	return newOIDC(func() *settings.AuthOIDC { return settings.EffectiveAuthOIDC(d.Catalog) },
		func() *settings.AuthSCIM { return settings.AuthSCIMFrom(d.Catalog) }, users, d.Sessions, d.CookieSecure)
}

func newOIDC(cfg func() *settings.AuthOIDC, scim func() *settings.AuthSCIM, users oidcUserStore, sessions sessionLogin, cookieSecure bool) *oidcDeps {
	od := &oidcDeps{cfg: cfg, scim: scim, users: users, sessions: sessions}
	od.rp = &oidc.RelyingParty[*user.User]{
		Config:       od.config,
		FlowCookie:   oidcFlowCookie,
		CookieSecure: cookieSecure,
		Provision:    od.resolveUser,
		Login:        od.login,
		OnDenied:     func(r *http.Request, f *oidc.Failure) { auditLoginDenied(r.Context(), f.Name) },
	}
	return od
}

// registerAuthOIDC mounts the two routes. Callers guarantee r already has
// the session middleware installed (Login needs scs context).
func registerAuthOIDC(r chi.Router, d Deps) {
	if d.Users == nil || d.Sessions == nil || d.Catalog == nil {
		return
	}
	mountOIDCRoutes(r, newOIDCDeps(d))
}

func mountOIDCRoutes(r chi.Router, od *oidcDeps) {
	r.Get("/auth/oidc/start", od.rp.Start)
	r.Get("/auth/oidc/callback", od.rp.Callback)
}

// MountOIDCCallbackRoot mounts GET /auth/callback at the listener root —
// the conventional shape for a registered redirect URI is
// https://<origin>/auth/callback, not an /api-prefixed path. It runs outside
// the /api group where Mount installs the session middleware, so it wraps
// the handler in that middleware itself (Login needs scs context). The
// /api/auth/oidc/callback mount stays for deployments whose IdP
// registration predates this path.
func MountOIDCCallbackRoot(r chi.Router, d Deps) {
	if d.Users == nil || d.Sessions == nil || d.Catalog == nil {
		return
	}
	mountOIDCCallbackRoot(r, d, newOIDCDeps(d))
}

func mountOIDCCallbackRoot(r chi.Router, d Deps, od *oidcDeps) {
	r.Group(func(g chi.Router) {
		g.Use(d.Sessions.Middleware)
		// Outside /api nothing installs the audit middleware, and the login
		// row the callback records needs one.
		if d.Audit != nil {
			g.Use(audit.Middleware(d.Audit, d.TrustedProxies))
		}
		g.Get("/auth/callback", od.rp.Callback)
	})
}

// config renders the auth:oidc section for the relying party; nil when disabled.
func (od *oidcDeps) config() *oidc.Config {
	cfg := od.cfg()
	if !cfg.Enabled {
		return nil
	}
	c := &oidc.Config{
		Issuer:       cfg.Issuer,
		ClientID:     cfg.ClientID,
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.EffectiveScopes(),
		AuthParams:   cfg.AuthParams,
		GroupsClaim:  cfg.EffectiveGroupsClaim(),
		PostLoginURL: cfg.PostLoginURL,
	}
	if cfg.ClientSecretEnv != "" {
		c.ClientSecret = os.Getenv(cfg.ClientSecretEnv)
	}
	return c
}

// auditLoginDenied records a failed SSO login. The subject (or the username,
// once one is known) is all the identity there is on these paths.
func auditLoginDenied(ctx context.Context, name string) {
	audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", Name: name},
		audit.StatusDenied, audit.Actor{Kind: audit.ActorAnonymous, Name: name})
}

// oidcSubject is the user row's key for an IdP identity: the configured issuer and the token's sub. It is stored as written, because an older binary matches it exactly on a rollback; lookups ignore trailing slashes on the issuer.
func oidcSubject(c oidc.Claims) string { return c.Issuer + "|" + c.Subject }

// resolveUser finds the user for an OIDC identity, linking it to a SCIM-provisioned account or provisioning one when registration is open, and refuses disabled accounts.
func (od *oidcDeps) resolveUser(ctx context.Context, c oidc.Claims) (*user.User, error) {
	subject := oidcSubject(c)
	u, err := od.users.ByOIDCSubject(ctx, subject)
	if err != nil {
		slog.Error("oidc callback: user lookup failed", "err", err)
		return nil, &oidc.Failure{Status: http.StatusInternalServerError, Message: "user lookup failed", Name: subject, Err: err}
	}
	if u == nil {
		if u, err = od.linkProvisioned(ctx, c); err != nil {
			return nil, err
		}
	}
	if u == nil {
		if !od.cfg().OpenRegistration() {
			return nil, &oidc.Failure{Status: http.StatusForbidden, Message: "no account for this identity (registration is closed)", Name: subject}
		}
		u, err = od.provision(ctx, subject, c)
		// Linking a sign-in to an existing account is an administrator's decision, never an email match.
		if errors.Is(err, directory.ErrEmailTaken) {
			return nil, &oidc.Failure{Status: http.StatusForbidden, Message: "an account with this email already exists; an administrator must link it to this sign-in", Name: subject, Err: err}
		}
		if err != nil {
			slog.Error("oidc callback: provision failed", "err", err)
			return nil, &oidc.Failure{Status: http.StatusInternalServerError, Message: "account provisioning failed", Name: subject, Err: err}
		}
		slog.Info("oidc: user auto-provisioned", "username", u.Username)
	}
	if u.Disabled {
		// The callback renders plain text, so it carries the status and message of accountDisabledError but not its code.
		return nil, &oidc.Failure{Status: http.StatusUnauthorized, Message: accountDisabledError().Err.Message, Name: u.Username}
	}
	return u, nil
}

// linkProvisioned returns the SCIM-provisioned account a first sign-in belongs to, adding the sign-in's identity to it; nil when SCIM is off or nothing matches. The SCIM client and the OIDC provider are one identity provider, so the provider's own id for the user (the auth:scim link claim) identifies the account; an email never does. A disabled match is returned unlinked, for the caller to refuse.
func (od *oidcDeps) linkProvisioned(ctx context.Context, c oidc.Claims) (*user.User, error) {
	section := od.scim()
	if !section.Enabled {
		return nil, nil
	}
	subject := oidcSubject(c)
	externalID := c.StringClaim(section.EffectiveLinkClaim())
	if externalID == "" {
		return nil, nil
	}
	matches, err := od.users.UnlinkedByExternalID(ctx, externalID)
	if err != nil {
		slog.Error("oidc callback: provisioned account lookup failed", "err", err)
		return nil, &oidc.Failure{Status: http.StatusInternalServerError, Message: "user lookup failed", Name: subject, Err: err}
	}
	switch {
	case len(matches) == 0:
		return nil, nil
	case len(matches) > 1:
		slog.Error("oidc callback: sign-in matches more than one provisioned account", "claim", section.EffectiveLinkClaim(), "accounts", len(matches))
		return nil, &oidc.Failure{Status: http.StatusConflict, Message: "this sign-in matches more than one provisioned account; an administrator must resolve it", Name: subject}
	}
	u := matches[0]
	if u.Disabled {
		return u, nil
	}
	err = od.users.AddFirstIdentity(ctx, u.ID, directory.Identity{Issuer: c.Issuer, Subject: c.Subject})
	if errors.Is(err, directory.ErrConflict) {
		// A concurrent first sign-in with this identity linked the account first.
		if again, lerr := od.users.ByOIDCSubject(ctx, subject); lerr == nil && again != nil {
			return again, nil
		}
		return nil, &oidc.Failure{Status: http.StatusConflict, Message: "the provisioned account is linked to another sign-in", Name: subject, Err: err}
	}
	if err != nil {
		slog.Error("oidc callback: account link failed", "err", err)
		return nil, &oidc.Failure{Status: http.StatusInternalServerError, Message: "account link failed", Name: subject, Err: err}
	}
	u.OIDCSubject = subject
	slog.Info("oidc: sign-in linked to a provisioned account", "username", u.Username)
	owner := meta.Owner{Kind: meta.OwnerSystem}
	audit.RecordExtra(ctx, audit.Row{
		Action:   "users.update",
		Resource: audit.Resource{Kind: "user", ID: u.ID, Name: u.Username, Owner: &owner},
		Fields:   []string{"identities"},
	})
	return u, nil
}

// login mints the relay session for u and records the allowed login.
func (od *oidcDeps) login(_ http.ResponseWriter, r *http.Request, u *user.User, c oidc.Claims) error {
	ctx := r.Context()
	if err := od.sessions.LoginOIDC(ctx, u.ID, u.Username, oidcSubject(c), c.SessionID, c.Groups, u.Roles...); err != nil {
		slog.Error("oidc callback: session create failed", "err", err)
		return &oidc.Failure{Status: http.StatusInternalServerError, Message: "session create failed", Name: u.Username, Err: err}
	}
	audit.Record(ctx, "auth.login",
		audit.Resource{Kind: "user", ID: u.ID, Name: u.Username}, audit.StatusAllowed,
		audit.Actor{Kind: audit.ActorUser, ID: u.ID, Name: u.Username})
	return nil
}

// provision creates a user row for a first-time OIDC login. Username derives from preferred_username, else the email local part, else the literal "user", with a collision suffix.
func (od *oidcDeps) provision(ctx context.Context, subject string, c oidc.Claims) (*user.User, error) {
	base := c.PreferredUsername
	if base == "" && c.Email != "" {
		base = c.Email[:strings.IndexByte(c.Email+"@", '@')]
	}
	base = slug.From(base)
	if base == "" {
		base = "user"
	}
	username := slug.Unique(base, func(candidate string) bool {
		u, err := od.users.ByUsername(ctx, candidate)
		return err != nil || u != nil // on lookup error assume taken — collide forward, never overwrite
	})
	u := &user.User{
		ID:          ids.New(),
		Username:    username,
		Email:       c.Email,
		OIDCSubject: subject,
	}
	if err := od.users.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

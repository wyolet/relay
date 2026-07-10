// OIDC login for the control plane: GET /auth/oidc/start redirects to the
// configured provider's hosted login; GET /auth/oidc/callback exchanges the
// authorization code and mints the same scs session password login mints,
// so everything downstream of login is identical for both paths.
//
// These are raw chi handlers, not huma operations: they speak browser
// redirects and cookies, not JSON.
//
// The provider is generic OIDC (issuer discovery via sdk/oauth). Per-dance
// state + PKCE verifier travel in a short-lived HttpOnly SameSite=Lax
// cookie rather than the session: the callback arrives as a cross-site
// top-level navigation, which a SameSite=Strict session cookie does not
// accompany.
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/slug"
	"github.com/wyolet/relay/sdk/oauth"
)

const (
	oidcFlowCookie = "relay_oidc_flow"
	oidcFlowTTL    = 10 * time.Minute
)

// oidcUserStore is the narrow user-store surface the callback needs.
// *user.Store satisfies it; tests supply a fake.
type oidcUserStore interface {
	ByUsername(ctx context.Context, username string) (*user.User, error)
	ByOIDCSubject(ctx context.Context, subject string) (*user.User, error)
	Upsert(ctx context.Context, u *user.User) error
}

// sessionLogin is the slice of session.Manager the callback needs.
type sessionLogin interface {
	LoginOIDC(ctx context.Context, userID, username, oidcSubject, idpSessionID string, roles ...string) error
}

// oidcDeps carries the seams the two handlers use, injectable for tests.
type oidcDeps struct {
	cfg          func() *settings.AuthOIDC
	users        oidcUserStore
	sessions     sessionLogin
	cookieSecure bool

	// discover resolves issuer metadata; defaults to sdk/oauth discovery
	// with a small per-process cache. Tests point it at a fake IdP.
	discover func(ctx context.Context, pc oauth.ProviderConfig) (oauth.ProviderConfig, error)
}

func newOIDCDeps(d Deps) *oidcDeps {
	return &oidcDeps{
		cfg:          func() *settings.AuthOIDC { return settings.EffectiveAuthOIDC(d.Catalog) },
		users:        d.Users,
		sessions:     d.Sessions,
		cookieSecure: d.CookieSecure,
		discover:     cachedDiscover(),
	}
}

// registerAuthOIDC mounts the two routes. Callers guarantee r already has
// the session middleware installed (Login needs scs context).
func registerAuthOIDC(r chi.Router, d Deps) {
	if d.Users == nil || d.Sessions == nil || d.Catalog == nil {
		return
	}
	od := newOIDCDeps(d)
	r.Get("/auth/oidc/start", od.start)
	r.Get("/auth/oidc/callback", od.callback)
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
	od := newOIDCDeps(d)
	r.Group(func(g chi.Router) {
		g.Use(d.Sessions.Middleware)
		g.Get("/auth/callback", od.callback)
	})
}

// flowState is the per-dance payload carried in the flow cookie.
type flowState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
}

func (od *oidcDeps) start(w http.ResponseWriter, r *http.Request) {
	cfg := od.cfg()
	if !cfg.Enabled {
		http.Error(w, "oidc login is not enabled", http.StatusNotFound)
		return
	}
	pc, err := od.provider(r.Context(), cfg)
	if err != nil {
		slog.Error("oidc start: discovery failed", "issuer", cfg.Issuer, "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "entropy unavailable", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(buf)

	authURL, verifier := oauth.New(pc.OAuth2()).AuthorizeURL(state, pc.AuthCodeOptions()...)
	raw, _ := json.Marshal(flowState{State: state, Verifier: verifier})
	http.SetCookie(w, &http.Cookie{
		Name:     oidcFlowCookie,
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     "/",
		MaxAge:   int(oidcFlowTTL.Seconds()),
		HttpOnly: true,
		Secure:   od.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (od *oidcDeps) callback(w http.ResponseWriter, r *http.Request) {
	cfg := od.cfg()
	if !cfg.Enabled {
		http.Error(w, "oidc login is not enabled", http.StatusNotFound)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		slog.Warn("oidc callback: provider returned error", "error", e,
			"description", r.URL.Query().Get("error_description"))
		http.Error(w, "login failed at identity provider: "+e, http.StatusBadGateway)
		return
	}

	fs, ok := od.takeFlowCookie(w, r)
	if !ok {
		http.Error(w, "login flow expired — start again", http.StatusBadRequest)
		return
	}
	qstate := r.URL.Query().Get("state")
	if qstate == "" || subtle.ConstantTimeCompare([]byte(qstate), []byte(fs.State)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	pc, err := od.provider(r.Context(), cfg)
	if err != nil {
		slog.Error("oidc callback: discovery failed", "issuer", cfg.Issuer, "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	oc := pc.OAuth2()
	if cfg.ClientSecretEnv != "" {
		oc.ClientSecret = os.Getenv(cfg.ClientSecretEnv)
	}
	tok, err := oauth.New(oc).Exchange(r.Context(), code, fs.Verifier)
	if err != nil {
		slog.Error("oidc callback: code exchange failed", "err", err)
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}

	claims, err := idTokenClaims(tok)
	if err != nil {
		slog.Error("oidc callback: id_token unusable", "err", err)
		http.Error(w, "identity provider returned no usable identity", http.StatusBadGateway)
		return
	}
	subject := cfg.Issuer + "|" + claims.Sub

	ctx := r.Context()
	u, err := od.users.ByOIDCSubject(ctx, subject)
	if err != nil {
		slog.Error("oidc callback: user lookup failed", "err", err)
		http.Error(w, "user lookup failed", http.StatusInternalServerError)
		return
	}
	if u == nil {
		if !cfg.OpenRegistration() {
			http.Error(w, "no account for this identity (registration is closed)", http.StatusForbidden)
			return
		}
		u, err = od.provision(ctx, subject, claims)
		if err != nil {
			slog.Error("oidc callback: provision failed", "err", err)
			http.Error(w, "account provisioning failed", http.StatusInternalServerError)
			return
		}
		slog.Info("oidc: user auto-provisioned", "username", u.Username)
	}
	if u.Disabled {
		http.Error(w, "account disabled", http.StatusForbidden)
		return
	}

	if err := od.sessions.LoginOIDC(ctx, u.ID, u.Username, subject, claims.Sid, u.Roles...); err != nil {
		slog.Error("oidc callback: session create failed", "err", err)
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	target := cfg.PostLoginURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// provider fills endpoints from issuer metadata and shapes the oauth config.
func (od *oidcDeps) provider(ctx context.Context, cfg *settings.AuthOIDC) (oauth.ProviderConfig, error) {
	return od.discover(ctx, oauth.ProviderConfig{
		Issuer:      cfg.Issuer,
		ClientID:    cfg.ClientID,
		RedirectURI: cfg.RedirectURL,
		Scopes:      cfg.EffectiveScopes(),
		AuthParams:  cfg.AuthParams,
	})
}

// takeFlowCookie reads, decodes, and expires the per-dance cookie.
func (od *oidcDeps) takeFlowCookie(w http.ResponseWriter, r *http.Request) (flowState, bool) {
	var fs flowState
	c, err := r.Cookie(oidcFlowCookie)
	if err != nil || c.Value == "" {
		return fs, false
	}
	http.SetCookie(w, &http.Cookie{
		Name: oidcFlowCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: od.cookieSecure, SameSite: http.SameSiteLaxMode,
	})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return fs, false
	}
	if err := json.Unmarshal(raw, &fs); err != nil || fs.State == "" || fs.Verifier == "" {
		return flowState{}, false
	}
	return fs, true
}

// provision creates a user row for a first-time OIDC login. Username derives
// from the email local part (or the subject tail) with a collision suffix.
func (od *oidcDeps) provision(ctx context.Context, subject string, c *idClaims) (*user.User, error) {
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
	if err := od.users.Upsert(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// idClaims is the subset of ID-token claims login consumes. Sid is the IdP
// session id — stored on the relay session so a back-channel-logout receiver
// can find sessions minted from a given IdP session.
type idClaims struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Sid               string `json:"sid"`
}

// idTokenClaims extracts claims from the token response's id_token. The
// payload is decoded without signature verification: the token arrived on
// the code-exchange response over TLS with client authentication, so the
// channel — not the signature — is the trust anchor here. (Bearer-token
// auth on the API, where tokens arrive from the caller, must verify
// against JWKS; that surface is separate.)
// idTokenClaims extracts the caller's identity from the code-exchange
// response. Standard OIDC carries it as an id_token JWT; some hosted
// user-management providers instead inline a profile object on the token
// response ({"user": {"id": ..., "email": ...}}) — both are accepted, JWT
// first. The payload is decoded without signature verification: the token
// arrived on the code-exchange response over TLS with client
// authentication, so the channel — not the signature — is the trust anchor
// here. (Bearer-token auth on the API, where tokens arrive from the caller,
// must verify against JWKS; that surface is separate.)
func idTokenClaims(tok *oauth.Token) (*idClaims, error) {
	if raw, _ := tok.Extra("id_token").(string); raw != "" {
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			return nil, fmt.Errorf("id_token is not a JWT")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("id_token payload: %w", err)
		}
		var c idClaims
		if err := json.Unmarshal(payload, &c); err != nil {
			return nil, fmt.Errorf("id_token claims: %w", err)
		}
		if c.Sub == "" {
			return nil, fmt.Errorf("id_token has no sub")
		}
		return &c, nil
	}
	if m, ok := tok.Extra("user").(map[string]any); ok {
		c := &idClaims{}
		c.Sub, _ = m["id"].(string)
		c.Email, _ = m["email"].(string)
		if c.Sub != "" {
			return c, nil
		}
	}
	return nil, fmt.Errorf("token response carried neither an id_token nor an inline user profile (is the openid scope configured?)")
}

// cachedDiscover wraps sdk/oauth issuer discovery with a small per-process
// cache so each login dance doesn't refetch provider metadata.
func cachedDiscover() func(ctx context.Context, pc oauth.ProviderConfig) (oauth.ProviderConfig, error) {
	type entry struct {
		pc  oauth.ProviderConfig
		exp time.Time
	}
	var (
		mu    sync.Mutex
		cache = map[string]entry{}
	)
	return func(ctx context.Context, pc oauth.ProviderConfig) (oauth.ProviderConfig, error) {
		key := pc.Issuer + "|" + pc.ClientID + "|" + pc.RedirectURI
		mu.Lock()
		if e, ok := cache[key]; ok && time.Now().Before(e.exp) {
			mu.Unlock()
			return e.pc, nil
		}
		mu.Unlock()
		out, err := pc.Discover(ctx, http.DefaultClient)
		if err != nil {
			return out, err
		}
		mu.Lock()
		cache[key] = entry{pc: out, exp: time.Now().Add(time.Hour)}
		mu.Unlock()
		return out, nil
	}
}

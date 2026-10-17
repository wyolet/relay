// Package session is a cookie session manager over alexedwards/scs/v2: middleware that loads and saves the session, login with token rotation, logout, ending every session of one user, and an authn.Authenticator that reads the caller from the loaded session.
//
// Storage is the product's: it implements Store, which is scs's store plus a per-user index of session tokens. Verifying credentials, and deciding what the caller may do, happen elsewhere; this package only records who logged in and answers who a request's session belongs to.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/wyolet/relay/auth/principal"
)

// Keys of the stored session payload. Changing one logs out every existing session.
const (
	keyUserID      = "user_id"
	keyUsername    = "username"
	keyRoles       = "roles"
	keyOIDCSubject = "oidc_subject"
	keyOIDCSid     = "oidc_sid"
	keyGroups      = "idp_groups"
)

const (
	defaultCookieName = "session"
	defaultLifetime   = 24 * time.Hour
)

// Store holds session payloads by token (scs.Store: Find, Commit, Delete) and an index of the tokens each user holds, which DestroyUser walks.
type Store interface {
	scs.Store
	// IndexUserToken records that token belongs to userID, for ttl.
	IndexUserToken(ctx context.Context, userID, token string, ttl time.Duration) error
	// UserTokens lists the tokens indexed under userID.
	UserTokens(ctx context.Context, userID string) ([]string, error)
	// DeleteUserToken deletes the session token names and its entry in userID's index.
	DeleteUserToken(ctx context.Context, userID, token string) error
}

// Options configure a Manager. The cookie is always HttpOnly and scoped to Path=/.
type Options struct {
	// CookieName defaults to "session".
	CookieName string
	// Secure marks the cookie HTTPS-only; set it everywhere but local development.
	Secure bool
	// SameSite defaults to Strict. A login or consent flow that returns through the identity provider needs Lax, or the browser withholds the cookie on the final redirect.
	SameSite http.SameSite
	// Lifetime is the absolute session lifetime; there is no idle timeout. Defaults to 24h.
	Lifetime time.Duration
	// Subjects renders the binding subjects of the session's principal, which carries its ID, Issuer, Subject, Groups (as the identity provider asserted them) and Roles. It runs on every Authenticate; an error fails the request as a server failure. Nil leaves Principal.Subjects empty.
	Subjects func(ctx context.Context, p *principal.Principal) ([]string, error)
}

// Manager is the session layer. Wrap the routes that use sessions with Middleware; Login, Logout and Authenticate act on the session it loaded.
type Manager struct {
	sm       *scs.SessionManager
	store    Store
	subjects func(ctx context.Context, p *principal.Principal) ([]string, error)
}

// New returns a Manager that keeps sessions in store.
func New(store Store, opts Options) *Manager {
	sm := scs.New()
	sm.Lifetime = opts.Lifetime
	if sm.Lifetime <= 0 {
		sm.Lifetime = defaultLifetime
	}
	sm.IdleTimeout = 0
	sm.Cookie.Name = opts.CookieName
	if sm.Cookie.Name == "" {
		sm.Cookie.Name = defaultCookieName
	}
	sm.Cookie.HttpOnly = true
	sm.Cookie.Secure = opts.Secure
	sm.Cookie.SameSite = opts.SameSite
	if sm.Cookie.SameSite == 0 {
		sm.Cookie.SameSite = http.SameSiteStrictMode
	}
	sm.Cookie.Path = "/"
	sm.Store = store
	// scs's default ErrorFunc logs the bare error through the stdlib logger and writes a text/plain 500. On the commit path scs calls this mid-response and then still writes the handler's own status; onceWriter absorbs that duplicate and any later body.
	sm.ErrorFunc = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("session middleware failure", "err", err, "method", r.Method, "path", r.URL.Path)
		const body = `{"title":"Internal Server Error","status":500,"detail":"session layer failure"}`
		if ow, ok := w.(*onceWriter); ok {
			ow.fail(http.StatusInternalServerError, "application/problem+json", []byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}
	return &Manager{sm: sm, store: store, subjects: opts.Subjects}
}

// Middleware wraps h with scs's LoadAndSave: it loads the session named by the cookie and persists changes with the response.
func (m *Manager) Middleware(h http.Handler) http.Handler {
	inner := m.sm.LoadAndSave(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(&onceWriter{ResponseWriter: w, req: r}, r)
	})
}

// Authenticate implements authn.Authenticator over the session Middleware loaded for r, which must run first. A request with no session, or one that expired or never existed, is not claimed: it continues as anonymous, as a missing cookie would.
func (m *Manager) Authenticate(r *http.Request) (*principal.Principal, bool, error) {
	ctx := r.Context()
	uid := m.sm.GetString(ctx, keyUserID)
	if uid == "" {
		return nil, false, nil
	}
	p := &principal.Principal{
		ID:         uid,
		Kind:       principal.KindUser,
		Username:   m.sm.GetString(ctx, keyUsername),
		SessionID:  sessionID(m.sm.Token(ctx)),
		Credential: principal.Credential{Kind: principal.CredentialSession},
	}
	if issuer, subject, ok := strings.Cut(m.sm.GetString(ctx, keyOIDCSubject), "|"); ok {
		p.Issuer, p.Subject = issuer, subject
	}
	if raw := m.sm.GetString(ctx, keyRoles); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.Roles)
	}
	if raw := m.sm.GetString(ctx, keyGroups); raw != "" {
		_ = json.Unmarshal([]byte(raw), &p.Groups)
	}
	if m.subjects != nil {
		subjects, err := m.subjects(ctx, p)
		if err != nil {
			slog.Error("session subjects failure", "err", err, "method", r.Method, "path", r.URL.Path)
			return nil, false, fmt.Errorf("session subjects: %w", err)
		}
		p.Subjects = subjects
	}
	return p, true, nil
}

// sessionID derives the non-secret id a principal carries from the session's bearer token. The token holds 256 random bits, so an unkeyed hash cannot be reversed, and SQL can recompute the same id for rows that stored the token.
func sessionID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// Login records userID, username and roles in the current session, rotating its token to prevent session fixation. Call it after the credentials are verified.
func (m *Manager) Login(ctx context.Context, userID, username string, roles ...string) error {
	if err := m.sm.RenewToken(ctx); err != nil {
		return err
	}
	// RenewToken keeps the session's data, and a login on a browser that still holds another account's session must not inherit its roles, IdP groups or subject.
	if err := m.sm.Clear(ctx); err != nil {
		return err
	}
	m.sm.Put(ctx, keyUserID, userID)
	m.sm.Put(ctx, keyUsername, username)
	if len(roles) > 0 {
		b, _ := json.Marshal(roles)
		m.sm.Put(ctx, keyRoles, string(b))
	}
	if userID == "" {
		return nil
	}
	return m.store.IndexUserToken(ctx, userID, m.sm.Token(ctx), m.sm.Lifetime)
}

// LoginOIDC is Login for an identity-provider login. It also records the IdP subject ("issuer|sub") and the IdP session id (the id_token sid claim), the keys a back-channel logout needs to find the sessions minted from one IdP session, and the groups the IdP asserted, which the session's principal carries for the rest of its life.
func (m *Manager) LoginOIDC(ctx context.Context, userID, username, oidcSubject, idpSessionID string, groups []string, roles ...string) error {
	if err := m.Login(ctx, userID, username, roles...); err != nil {
		return err
	}
	if len(groups) > 0 {
		b, _ := json.Marshal(groups)
		m.sm.Put(ctx, keyGroups, string(b))
	}
	if oidcSubject != "" {
		m.sm.Put(ctx, keyOIDCSubject, oidcSubject)
	}
	if idpSessionID != "" {
		m.sm.Put(ctx, keyOIDCSid, idpSessionID)
	}
	return nil
}

// Logout destroys the current session.
func (m *Manager) Logout(ctx context.Context) error {
	return m.sm.Destroy(ctx)
}

// DestroyUser ends every session the user holds, wherever the store is shared. A session carries the roles it logged in with, so a role change or a disable has to end it rather than wait out its lifetime.
func (m *Manager) DestroyUser(ctx context.Context, userID string) error {
	tokens, err := m.store.UserTokens(ctx, userID)
	if err != nil {
		return err
	}
	for _, token := range tokens {
		if err := m.store.DeleteUserToken(ctx, userID, token); err != nil {
			return err
		}
	}
	return nil
}

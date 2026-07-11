// Package session wraps alexedwards/scs/v2 with a kv.Store-backed session
// store and the small amount of glue that turns scs payloads into our
// app/actor.Actor.
//
// Why scs? Mature, well-tested session manager that handles cookie
// attributes, ID generation, rotation, and expiry correctly. We supply the
// storage backend (kv.Store, the same abstraction used by rate-limits and
// the key-pool) and the typed payload schema.
//
// What's NOT in scope: authentication (password verification — see
// internal/identity.Verify) or authorization (see app/authz). This package
// only manages "is there a valid session, and what's in it."
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/kv"
)

const (
	cookieName     = "relay_session"
	keyUserID      = "user_id"
	keyUsername    = "username"
	keyRoles       = "roles"
	keyOIDCSubject = "oidc_subject"
	keyOIDCSid     = "oidc_sid"
	defaultExpiry  = 24 * time.Hour
)

// Manager is the session layer. Construct via New(); attach the chi
// middleware via Middleware(); use Login/Logout/Actor in handlers.
type Manager struct {
	sm *scs.SessionManager
}

// New constructs a Manager backed by store. Cookies use the supplied
// attributes; secure=true is recommended in any deployment running behind
// HTTPS (which is everything except local dev).
//
// keyPrefix is prepended to every kv key the manager writes — pick a
// namespace like "sess:" so it doesn't collide with rate-limits or
// key-pool state.
func New(store kv.Store, secure bool, keyPrefix string) *Manager {
	sm := scs.New()
	sm.Lifetime = defaultExpiry
	sm.IdleTimeout = 0 // no idle timeout — only absolute expiry
	sm.Cookie.Name = cookieName
	sm.Cookie.HttpOnly = true
	sm.Cookie.Secure = secure
	sm.Cookie.SameSite = http.SameSiteStrictMode
	sm.Cookie.Path = "/"
	sm.Store = &kvStore{kv: store, prefix: keyPrefix}
	// scs's default ErrorFunc logs the bare error via the stdlib logger —
	// which the slog bridge renders as an attribute-less INFO line — and
	// writes a text/plain 500. Ours keeps the failure attributable. On the
	// commit path scs invokes this mid-response and then still writes the
	// handler's original status; onceWriter absorbs that duplicate and any
	// post-error body.
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
	return &Manager{sm: sm}
}

// onceWriter guards the response writer under the session middleware.
// Two jobs: (1) absorb duplicate WriteHeader calls — scs's wrapper relays
// them to the stdlib writer, so every double-write in ANY control handler
// surfaces as a "superfluous WriteHeader ... (session.go)" warning that
// masks the real culprit; we suppress the duplicate and log the offender's
// method/path instead. (2) after ErrorFunc has produced an error response
// mid-request, drop the handler's remaining body bytes so the 500 payload
// isn't followed by junk.
type onceWriter struct {
	http.ResponseWriter
	req    *http.Request
	status int
	wrote  bool
	failed bool
}

func (o *onceWriter) WriteHeader(code int) {
	if o.wrote {
		slog.Warn("duplicate WriteHeader suppressed",
			"method", o.req.Method, "path", o.req.URL.Path,
			"first", o.status, "second", code)
		return
	}
	o.wrote, o.status = true, code
	o.ResponseWriter.WriteHeader(code)
}

func (o *onceWriter) Write(b []byte) (int, error) {
	if o.failed {
		// Pretend success: the handler keeps running harmlessly while the
		// client only ever sees the error response.
		return len(b), nil
	}
	if !o.wrote {
		o.wrote, o.status = true, http.StatusOK
	}
	return o.ResponseWriter.Write(b)
}

// fail emits an error response if the header hasn't gone out yet, then
// swallows everything the handler writes afterwards.
func (o *onceWriter) fail(code int, contentType string, body []byte) {
	if !o.wrote {
		o.Header().Set("Content-Type", contentType)
		o.WriteHeader(code)
		_, _ = o.ResponseWriter.Write(body)
	}
	o.failed = true
}

func (o *onceWriter) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// Middleware wraps h with scs's LoadAndSave middleware (reads cookie,
// loads session from kv, persists changes on response). It also reads the
// session payload after load and stamps an Actor onto the request context
// so handlers can call actor.From(ctx) directly.
func (m *Manager) Middleware(h http.Handler) http.Handler {
	inner := m.loadAndSave(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(&onceWriter{ResponseWriter: w, req: r}, r)
	})
}

func (m *Manager) loadAndSave(h http.Handler) http.Handler {
	return m.sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		uid := m.sm.GetString(ctx, keyUserID)
		if uid != "" {
			a := &actor.Actor{
				UserID:    uid,
				Username:  m.sm.GetString(ctx, keyUsername),
				SessionID: m.sm.Token(ctx),
			}
			if raw := m.sm.GetString(ctx, keyRoles); raw != "" {
				_ = json.Unmarshal([]byte(raw), &a.Roles)
			}
			ctx = actor.WithActor(ctx, a)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
}

// Login records userID/username (and any roles) into the current session,
// rotating the session ID to prevent session fixation. Call after
// credential validation.
func (m *Manager) Login(ctx context.Context, userID, username string, roles ...string) error {
	if err := m.sm.RenewToken(ctx); err != nil {
		return err
	}
	m.sm.Put(ctx, keyUserID, userID)
	m.sm.Put(ctx, keyUsername, username)
	if len(roles) > 0 {
		b, _ := json.Marshal(roles)
		m.sm.Put(ctx, keyRoles, string(b))
	}
	return nil
}

// LoginOIDC is Login for the OIDC path: it additionally records the IdP
// subject ("issuer|sub") and IdP session id (the id_token sid claim) on the
// session — the lookup keys a back-channel-logout receiver needs to find
// and destroy the relay sessions minted from a given IdP session.
func (m *Manager) LoginOIDC(ctx context.Context, userID, username, oidcSubject, idpSessionID string, roles ...string) error {
	if err := m.Login(ctx, userID, username, roles...); err != nil {
		return err
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

// kvStore adapts kv.Store to scs.Store. scs's interface is sync without a
// context, so we use a short timeout per call.
type kvStore struct {
	kv     kv.Store
	prefix string
}

type kvEntry struct {
	Data   []byte    `json:"d"`
	Expiry time.Time `json:"e"`
}

func (s *kvStore) key(token string) string { return s.prefix + token }

func (s *kvStore) Find(token string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := s.kv.Get(ctx, s.key(token))
	if err != nil {
		// scs convention: (nil, false, nil) for "no such session."
		if errors.Is(err, kv.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("session find (kv get): %w", err)
	}
	if raw == nil {
		return nil, false, nil
	}
	var e kvEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false, fmt.Errorf("session find (decode): %w", err)
	}
	if time.Now().After(e.Expiry) {
		return nil, false, nil
	}
	return e.Data, true, nil
}

func (s *kvStore) Commit(token string, b []byte, expiry time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := json.Marshal(kvEntry{Data: b, Expiry: expiry})
	if err != nil {
		return fmt.Errorf("session commit (encode): %w", err)
	}
	if err := s.kv.Set(ctx, s.key(token), raw, time.Until(expiry)); err != nil {
		return fmt.Errorf("session commit (kv set): %w", err)
	}
	return nil
}

func (s *kvStore) Delete(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.kv.Del(ctx, s.key(token)); err != nil {
		return fmt.Errorf("session delete (kv del): %w", err)
	}
	return nil
}

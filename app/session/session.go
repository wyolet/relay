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
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/kv"
)

const (
	cookieName    = "relay_session"
	keyUserID     = "user_id"
	keyUsername   = "username"
	defaultExpiry = 24 * time.Hour
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
	return &Manager{sm: sm}
}

// Middleware wraps h with scs's LoadAndSave middleware (reads cookie,
// loads session from kv, persists changes on response). It also reads the
// session payload after load and stamps an Actor onto the request context
// so handlers can call actor.From(ctx) directly.
func (m *Manager) Middleware(h http.Handler) http.Handler {
	return m.sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		uid := m.sm.GetString(ctx, keyUserID)
		if uid != "" {
			a := &actor.Actor{
				UserID:    uid,
				Username:  m.sm.GetString(ctx, keyUsername),
				SessionID: m.sm.Token(ctx),
			}
			ctx = actor.WithActor(ctx, a)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
}

// Login records userID/username into the current session, rotating the
// session ID to prevent session fixation. Call after credential validation.
func (m *Manager) Login(ctx context.Context, userID, username string) error {
	if err := m.sm.RenewToken(ctx); err != nil {
		return err
	}
	m.sm.Put(ctx, keyUserID, userID)
	m.sm.Put(ctx, keyUsername, username)
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
		return nil, false, err
	}
	if raw == nil {
		return nil, false, nil
	}
	var e kvEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, false, err
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
		return err
	}
	return s.kv.Set(ctx, s.key(token), raw, time.Until(expiry))
}

func (s *kvStore) Delete(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.kv.Del(ctx, s.key(token))
}

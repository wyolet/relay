// Package session is relay's cookie session layer: the auth module's session manager over a kv.Store, so every pod sharing the store shares the sessions.
//
// kv ops: a request carrying the cookie does one Get; one that changes its session adds a Set. Login adds a Set for the per-user index entry; DestroyUser does one Range plus two Dels per session.
//
// Out of scope: verifying credentials (internal/identity) and deciding access (app/authz).
package session

import (
	"time"

	"github.com/wyolet/relay/app/catalog"
	authsession "github.com/wyolet/relay/auth/session"
	"github.com/wyolet/relay/pkg/kv"
)

const (
	cookieName = "relay_session"
	lifetime   = 24 * time.Hour
)

// Manager is the session layer: Middleware loads the session, Login/Logout/DestroyUser change it, and Authenticate reads the caller from it.
type Manager = authsession.Manager

// New returns a Manager keeping sessions in store under keyPrefix (e.g. "sess:", so they don't collide with other kv consumers). secure marks the cookie HTTPS-only, which every deployment but local dev wants.
func New(store kv.Store, secure bool, keyPrefix string) *Manager {
	return authsession.New(&kvStore{kv: store, prefix: keyPrefix}, authsession.Options{
		CookieName: cookieName,
		Secure:     secure,
		Lifetime:   lifetime,
		Subjects:   catalog.UserSubjects,
	})
}

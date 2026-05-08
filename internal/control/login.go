// Package control hosts the control-plane HTTP surface — operator-facing
// endpoints that configure and inspect the Relay (login, identity, CRUD,
// reload). It is the counterpart to the data plane in cmd/relay.
//
// In v1 the control router runs as a second http.Server inside the relay
// binary on RELAY_CONTROL_PORT. The package boundary is deliberate: when
// the control plane splits into its own binary (cmd/relay-control), this
// package is moved as-is and a new main.go imports it directly.
package control

import (
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/wyolet/relay/internal/identity"
)

// CookieName is the cookie used to carry the admin/control session token.
const CookieName = "relay_admin"

// ErrInvalidCredentials is returned by ValidateLogin for any login failure
// (unknown user OR bad password). The caller must not distinguish the two
// cases in error responses — that would leak which half was wrong.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ValidateLogin looks up username and verifies password using a constant-time
// compare. When the user is missing, it still runs the compare against a
// fixed-length dummy so timing doesn't reveal user existence.
//
// Returns (user, nil) on success or (nil, ErrInvalidCredentials) on any
// failure.
func ValidateLogin(store *identity.Store, username, password string) (*identity.User, error) {
	if store == nil {
		return nil, ErrInvalidCredentials
	}
	const dummy = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	var expected string
	u, ok := store.ByName(username)
	if ok {
		expected = u.Spec.Password.Get()
	} else {
		expected = dummy
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(expected)) != 1 || !ok {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// NewSessionCookie builds the relay_admin cookie carrying the session token
// (today: the configured admin token; later: a per-user random session id).
func NewSessionCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}

// NewClearCookie builds the cookie that, when sent in a response, clears
// relay_admin in the browser.
func NewClearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   0,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}

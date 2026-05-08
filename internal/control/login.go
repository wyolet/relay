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
	"encoding/json"
	"net/http"

	"github.com/wyolet/relay/internal/identity"
)

const cookieName = "relay_admin"

// LoginDeps is the minimal set of dependencies the login handler needs.
type LoginDeps struct {
	// Identity is the resolved User store. Nil disables username/password login.
	Identity *identity.Store
	// SessionToken is the value written into the relay_admin cookie on
	// successful login. Today this is the configured RELAY_ADMIN_TOKEN so the
	// existing /admin/* gate continues to accept the cookie. When the gate
	// migrates to per-user sessions this becomes a per-login random token.
	SessionToken string
}

// LoginHandler returns POST /control/login.
//
// Body: {"username": "...", "password": "..."}
//
// Validates credentials against the identity store using constant-time
// comparison. On success sets relay_admin cookie (HttpOnly, Secure,
// SameSite=Strict, 24 h). 401 on bad creds, 400 on malformed body.
func LoginHandler(deps LoginDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Identity == nil || deps.SessionToken == "" {
			writeErr(w, http.StatusServiceUnavailable, "login not configured")
			return
		}

		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if body.Username == "" || body.Password == "" {
			writeErr(w, http.StatusBadRequest, "username and password required")
			return
		}

		// Look up by username. We always run the password compare even when
		// the user is missing — using a fixed-length dummy — so a 401 doesn't
		// leak which half of the credential pair was wrong.
		const dummy = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
		var expected string
		u, ok := deps.Identity.ByName(body.Username)
		if ok {
			expected = u.Spec.Password.Get()
		} else {
			expected = dummy
		}
		match := subtle.ConstantTimeCompare([]byte(body.Password), []byte(expected)) == 1
		if !ok || !match {
			writeErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    deps.SessionToken,
			Path:     "/",
			MaxAge:   86400,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"username": u.Metadata.Name,
			"roles":    u.Spec.Roles,
		})
	}
}

// LogoutHandler returns POST /control/logout. Clears relay_admin.
func LogoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   0,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

// WhoamiHandler returns GET /control/whoami. The cookie value isn't yet
// reversible to a user (it's the shared admin token); for now we just report
// authenticated status. Per-user sessions slot in here when the gate moves
// off the shared token.
func WhoamiHandler(sessionToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(sessionToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

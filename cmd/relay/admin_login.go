package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
)

// adminLoginCookieName is the cookie used to carry the admin session token.
const adminLoginCookieName = "relay_admin"

// adminLoginHandler handles POST /admin/login.
// This endpoint is NOT gated by adminTokenGate (chicken-and-egg).
// Returns 401 on wrong token (unlike other admin endpoints which return 404).
// On success, sets an HttpOnly Secure SameSite=Strict cookie valid for 24 h.
func adminLoginHandler(token string) http.HandlerFunc {
	tok := []byte(token)
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "could not read request body")
			return
		}

		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Token == "" {
			adminWriteErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "body must be {\"token\":\"<admin-token>\"}")
			return
		}

		if subtle.ConstantTimeCompare([]byte(body.Token), tok) != 1 {
			adminWriteErr(w, http.StatusUnauthorized, "authentication_error", "invalid_token", "invalid admin token")
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     adminLoginCookieName,
			Value:    body.Token,
			Path:     "/",
			MaxAge:   86400,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		adminWriteJSON(w, http.StatusOK, map[string]any{})
	}
}

// adminLogoutHandler handles POST /admin/logout.
// Clears the relay_admin cookie. Requires the cookie or header to already be present
// (gated by adminTokenGate) so that anonymous clients cannot probe the endpoint.
func adminLogoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     adminLoginCookieName,
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

// adminWhoamiHandler handles GET /admin/whoami.
// Gated by adminTokenGate. Returns {authenticated: true} on success.
func adminWhoamiHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminWriteJSON(w, http.StatusOK, map[string]any{"authenticated": true})
	}
}

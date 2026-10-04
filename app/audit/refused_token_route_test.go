package audit

import (
	"net/http"
	"testing"
)

// The token routes hang off /auth, so the path-derived fallback has to name
// them explicitly or a refused mint is recorded as auth.create.
func TestRefusedRouteNamesTheTokenActions(t *testing.T) {
	for _, tc := range []struct {
		method, path, wantAction, wantID string
	}{
		{http.MethodPost, "/api/auth/token", "tokens.mint", ""},
		{http.MethodPost, "/api/auth/token/revoke", "tokens.revoke", ""},
		{http.MethodDelete, "/api/auth/token/j-1", "tokens.revoke", "j-1"},
		{http.MethodPost, "/api/auth/token/revoke-all", "tokens.revoke-all", ""},
		{http.MethodPost, "/api/auth/token/keys/rotate", "tokens.rotate", ""},
	} {
		got, marked := refusedRoute(tc.method, tc.path, http.StatusUnauthorized)
		if !marked {
			t.Errorf("%s %s earned no row", tc.method, tc.path)
			continue
		}
		if got.Action != tc.wantAction {
			t.Errorf("%s %s action = %q, want %q", tc.method, tc.path, got.Action, tc.wantAction)
		}
		if got.Resource.Kind != "token" {
			t.Errorf("%s %s kind = %q, want token", tc.method, tc.path, got.Resource.Kind)
		}
		if got.Resource.ID != tc.wantID {
			t.Errorf("%s %s id = %q, want %q", tc.method, tc.path, got.Resource.ID, tc.wantID)
		}
	}
}

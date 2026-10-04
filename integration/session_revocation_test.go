//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/user"
)

// A session carries the roles it logged in with, so changing a user's roles
// or disabling them must end the sessions they already hold.
func TestIntegration_RoleChangeAndDisableEndLiveSessions(t *testing.T) {
	st := newStack(t)
	spare := st.seedLogin(t, "spare-admin", "pw-spare")
	if u, _ := st.users.Get(t.Context(), spare); u != nil {
		u.Roles = []string{user.RoleAdmin}
		if err := st.users.Upsert(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []string{`{"roles":[]}`, `{"disabled":true}`} {
		id := st.seedLogin(t, "op", "pw-op")
		u, _ := st.users.Get(t.Context(), id)
		u.Roles = []string{user.RoleAdmin}
		u.Disabled = false
		if err := st.users.Upsert(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		us := st.login(t, "op", "pw-op")
		if code, raw := us.do(http.MethodGet, "/api/auth/whoami", ""); code != http.StatusOK {
			t.Fatalf("whoami before %s = %d: %s", change, code, raw)
		}
		if code, raw := st.adminDo(http.MethodPut, "/api/users/by-id/"+id, change); code != http.StatusOK {
			t.Fatalf("PUT %s = %d: %s", change, code, raw)
		}
		if code, raw := us.do(http.MethodGet, "/api/auth/whoami", ""); code != http.StatusUnauthorized {
			t.Fatalf("whoami after %s = %d, want 401: %s", change, code, raw)
		}
		if err := st.users.Delete(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
}

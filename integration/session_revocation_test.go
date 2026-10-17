//go:build integration

package integration_test

import (
	"net/http"
	"net/url"
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/user"
)

// A session can survive its account's disable: the session purge after the write can fail, or a login can index its session after the purge ran. Once the disable reaches the snapshot, such a session is refused on every control route but logout, and a re-enable restores it.
func TestIntegration_DisabledAccountSessionIsRefused(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	id := st.seedLogin(t, "op", "pw-op")
	us := st.login(t, "op", "pw-op")
	// Writes through the user store, not the directory, so no session is purged: the session left behind is the one under test.
	setDisabled := func(disabled bool) {
		t.Helper()
		u, err := st.users.Get(t.Context(), id)
		if err != nil || u == nil {
			t.Fatalf("get user: %v", err)
		}
		u.Disabled = disabled
		if err := st.users.Upsert(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		st.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return s.UserDisabled(id) == disabled },
			"the user's disabled flag never reached the snapshot")
	}
	const disabledBody = `{"error":{"type":"invalid_request_error","message":"account is disabled"}}`

	if code, raw := us.do(http.MethodGet, "/api/auth/whoami", ""); code != http.StatusOK {
		t.Fatalf("whoami before disable = %d: %s", code, raw)
	}
	setDisabled(true)
	for _, path := range []string{"/api/auth/whoami", "/api/users"} {
		if code, raw := us.do(http.MethodGet, path, ""); code != http.StatusForbidden || string(raw) != disabledBody {
			t.Fatalf("GET %s with a disabled account's session = %d %s, want 403 %s", path, code, raw, disabledBody)
		}
	}
	if code, raw := st.adminDo(http.MethodGet, "/api/users", ""); code != http.StatusOK {
		t.Fatalf("admin token while the account is disabled = %d: %s", code, raw)
	}

	setDisabled(false)
	if code, raw := us.do(http.MethodGet, "/api/auth/whoami", ""); code != http.StatusOK {
		t.Fatalf("whoami after re-enable = %d: %s", code, raw)
	}

	setDisabled(true)
	base, err := url.Parse(st.control.URL)
	if err != nil {
		t.Fatal(err)
	}
	loggedOut := us.client.Jar.Cookies(base)
	if code, raw := us.do(http.MethodPost, "/api/auth/logout", ""); code >= 300 {
		t.Fatalf("logout with a disabled account's session = %d: %s", code, raw)
	}
	setDisabled(false)
	// Replay the cookie the browser held: logout must have ended the session server-side, not only cleared the cookie.
	req, err := http.NewRequest(http.MethodGet, st.control.URL+"/api/auth/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loggedOut {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(loggedOut) == 0 || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("whoami with the logged-out cookie (%d cookies) = %d, want 401", len(loggedOut), resp.StatusCode)
	}
}

// A session carries the roles it logged in with, so changing a user's roles
// or disabling them must end the sessions they already hold.
func TestIntegration_RoleChangeAndDisableEndLiveSessions(t *testing.T) {
	t.Parallel()
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

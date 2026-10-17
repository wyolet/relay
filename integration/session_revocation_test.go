//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/user"
)

const (
	accountDisabledBody = `{"error":{"type":"authentication_error","code":"account_disabled","message":"account is disabled; contact your administrator"}}`
	unauthenticatedBody = `{"error":{"type":"authentication_error","message":"unauthenticated"}}`
)

// isAccountDisabled reports whether raw is the account_disabled error. A huma handler's error body also carries a "$schema" link, which this ignores.
func isAccountDisabled(raw []byte) bool {
	var got, want struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal([]byte(accountDisabledBody), &want) != nil {
		return false
	}
	return reflect.DeepEqual(got.Error, want.Error)
}

// cookiesOf is the cookies a logged-in browser holds for the control plane.
func (s *stack) cookiesOf(t *testing.T, us *userSession) []*http.Cookie {
	t.Helper()
	base, err := url.Parse(s.control.URL)
	if err != nil {
		t.Fatal(err)
	}
	return us.client.Jar.Cookies(base)
}

// doWithCookies sends a control request carrying exactly cookies, the way a browser that kept them would.
func (s *stack) doWithCookies(t *testing.T, method, path string, cookies []*http.Cookie) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, s.control.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// A session can survive its account's disable: the session purge after the write can fail, or a login can index its session after the purge ran. Once the disable reaches the snapshot, that session's next request signs it out with account_disabled, password login says the same once the password verified, and a re-enable lets the account sign in again.
func TestIntegration_DisabledAccountIsSignedOut(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	id := st.seedLogin(t, "op", "pw-op")
	us := st.login(t, "op", "pw-op")
	held := st.cookiesOf(t, us)
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

	if code, raw := us.do(http.MethodGet, "/api/users", ""); code != http.StatusOK {
		t.Fatalf("GET /api/users before disable = %d: %s", code, raw)
	}
	setDisabled(true)
	if code, raw := us.do(http.MethodGet, "/api/users", ""); code != http.StatusUnauthorized || string(raw) != accountDisabledBody {
		t.Fatalf("disabled account's session = %d %s, want 401 %s", code, raw, accountDisabledBody)
	}
	if code, raw := st.doWithCookies(t, http.MethodGet, "/api/users", held); code != http.StatusUnauthorized || string(raw) != unauthenticatedBody {
		t.Fatalf("the ended session's cookie again = %d %s, want 401 %s", code, raw, unauthenticatedBody)
	}
	if code, raw := st.adminDo(http.MethodGet, "/api/users", ""); code != http.StatusOK {
		t.Fatalf("admin token while the account is disabled = %d: %s", code, raw)
	}

	fresh := &userSession{t: t, base: st.control.URL, client: &http.Client{}}
	if code, raw := fresh.do(http.MethodPost, "/api/auth/login", `{"username":"op","password":"wrong"}`); code != http.StatusUnauthorized || !strings.Contains(string(raw), `"invalid credentials"`) {
		t.Fatalf("disabled account, wrong password = %d %s, want 401 invalid credentials", code, raw)
	}
	if code, raw := fresh.do(http.MethodPost, "/api/auth/login", `{"username":"op","password":"pw-op"}`); code != http.StatusUnauthorized || !isAccountDisabled(raw) {
		t.Fatalf("disabled account, right password = %d %s, want 401 %s", code, raw, accountDisabledBody)
	}

	// The same browser signs in as another account.
	st.seedLogin(t, "other", "pw-other")
	if code, raw := us.do(http.MethodPost, "/api/auth/login", `{"username":"other","password":"pw-other"}`); code != http.StatusOK {
		t.Fatalf("login as another account on the signed-out browser = %d: %s", code, raw)
	}

	setDisabled(false)
	again := st.login(t, "op", "pw-op")
	if code, raw := again.do(http.MethodGet, "/api/users", ""); code != http.StatusOK {
		t.Fatalf("fresh login after re-enable = %d: %s", code, raw)
	}
}

// A disabled flag written outside every store, with no token_version bump, still reaches the snapshot over NOTIFY and signs the account's session out.
func TestIntegration_RawDisableReachesTheSnapshot(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	id := st.seedLogin(t, "op", "pw-op")
	us := st.login(t, "op", "pw-op")
	// The listener applies NOTIFYs on a one-second debounce tick. A reload still queued from the seed write would read the raw disable too and hide whether the UPDATE notified, so let it drain first.
	st.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { _, ok := s.TokenVersion(id); return ok },
		"the new user never reached the snapshot")
	time.Sleep(1500 * time.Millisecond)
	if _, err := testPool(t, st.dsn).Exec(t.Context(), `UPDATE users SET disabled = true WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	st.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return s.UserDisabled(id) },
		"a raw disable never reached the snapshot")
	if code, raw := us.do(http.MethodGet, "/api/users", ""); code != http.StatusUnauthorized || string(raw) != accountDisabledBody {
		t.Fatalf("session after a raw disable = %d %s, want 401 %s", code, raw, accountDisabledBody)
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

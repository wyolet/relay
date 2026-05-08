package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyolet/relay/internal/identity"
)

func setupStore(t *testing.T) *identity.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RELAY_TEST_PW", "hunter2hunter2")
	body := `apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: admin}
spec:
  username: admin
  email: admin@example.com
  password: {valueFrom: {env: RELAY_TEST_PW}}
  roles: [admin]
`
	if err := os.WriteFile(filepath.Join(dir, "u.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := identity.LoadYAML(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, h http.Handler, path, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestLogin_Success(t *testing.T) {
	r := NewRouter(LoginDeps{Identity: setupStore(t), SessionToken: "tok"})
	resp := post(t, r, "/control/login", `{"username":"admin","password":"hunter2hunter2"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value == "tok" && c.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("relay_admin cookie not set or wrong value")
	}
}

func TestLogin_BadPassword(t *testing.T) {
	r := NewRouter(LoginDeps{Identity: setupStore(t), SessionToken: "tok"})
	resp := post(t, r, "/control/login", `{"username":"admin","password":"wrong"}`)
	if resp.StatusCode != 401 {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestLogin_UnknownUser(t *testing.T) {
	r := NewRouter(LoginDeps{Identity: setupStore(t), SessionToken: "tok"})
	resp := post(t, r, "/control/login", `{"username":"nobody","password":"hunter2hunter2"}`)
	if resp.StatusCode != 401 {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestLogin_BadBody(t *testing.T) {
	r := NewRouter(LoginDeps{Identity: setupStore(t), SessionToken: "tok"})
	resp := post(t, r, "/control/login", `not json`)
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestLogin_NotConfigured(t *testing.T) {
	r := NewRouter(LoginDeps{Identity: nil, SessionToken: ""})
	resp := post(t, r, "/control/login", `{"username":"admin","password":"x"}`)
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
}

func TestWhoami_RequiresCookie(t *testing.T) {
	r := NewRouter(LoginDeps{Identity: setupStore(t), SessionToken: "tok"})
	req := httptest.NewRequest(http.MethodGet, "/control/whoami", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("anon whoami: status=%d want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/control/whoami", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "tok"})
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("authed whoami: status=%d want 200", rec.Code)
	}
}

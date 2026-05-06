package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

const loginTestToken = "login-test-secret-token"

// buildLoginTestServer mounts only the login/logout/whoami endpoints (no PG needed).
// It also mounts GET /admin/providers (via the token gate) as a regression target.
func buildLoginTestServer(tok string) http.Handler {
	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))

	gate := adminTokenGate(tok)

	// Login — unauthenticated
	r.Post("/admin/login", adminLoginHandler(tok))
	// Logout and whoami — gated
	r.With(gate).Post("/admin/logout", adminLogoutHandler())
	r.With(gate).Get("/admin/whoami", adminWhoamiHandler())
	// Regression: generic gated endpoint
	r.With(gate).Get("/admin/providers", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		adminWriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
	}))

	return r
}

func doLoginReq(t *testing.T, srv *httptest.Server, method, path string, body any, cookies []*http.Cookie) *http.Response {
	t.Helper()
	var buf *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestAdminLogin_CorrectToken_200_SetsCookie(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	resp := doLoginReq(t, srv, http.MethodPost, "/admin/login", map[string]any{"token": loginTestToken}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	c := findCookie(resp, adminLoginCookieName)
	if c == nil {
		t.Fatal("want relay_admin cookie, not set")
	}
	if c.Value != loginTestToken {
		t.Errorf("cookie value: want %q, got %q", loginTestToken, c.Value)
	}
	if !c.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("cookie must be Secure")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite: want Strict, got %v", c.SameSite)
	}
	if c.MaxAge != 86400 {
		t.Errorf("cookie MaxAge: want 86400, got %d", c.MaxAge)
	}
}

func TestAdminLogin_WrongToken_401(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	resp := doLoginReq(t, srv, http.MethodPost, "/admin/login", map[string]any{"token": "bad-token"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestAdminLogin_EmptyBody_400(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	// Empty token field.
	resp := doLoginReq(t, srv, http.MethodPost, "/admin/login", map[string]any{"token": ""}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty token: want 400, got %d", resp.StatusCode)
	}
}

func TestAdminLogin_MalformedBody_400(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/login", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json: want 400, got %d", resp.StatusCode)
	}
}

func TestAdminLogout_WithCookie_204_ClearesCookie(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	// Login first to get cookie.
	loginResp := doLoginReq(t, srv, http.MethodPost, "/admin/login", map[string]any{"token": loginTestToken}, nil)
	c := findCookie(loginResp, adminLoginCookieName)
	if c == nil {
		t.Fatal("no cookie from login")
	}

	// Logout.
	resp := doLoginReq(t, srv, http.MethodPost, "/admin/logout", nil, []*http.Cookie{c})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}

	// Set-Cookie on logout response should clear the cookie (MaxAge=0).
	cleared := findCookie(resp, adminLoginCookieName)
	if cleared == nil {
		t.Fatal("want Set-Cookie on logout response")
	}
	if cleared.MaxAge != 0 {
		t.Errorf("want MaxAge=0 on logout cookie, got %d", cleared.MaxAge)
	}
}

func TestAdminWhoami_WithCookie_200(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	loginResp := doLoginReq(t, srv, http.MethodPost, "/admin/login", map[string]any{"token": loginTestToken}, nil)
	c := findCookie(loginResp, adminLoginCookieName)
	if c == nil {
		t.Fatal("no cookie from login")
	}

	resp := doLoginReq(t, srv, http.MethodGet, "/admin/whoami", nil, []*http.Cookie{c})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["authenticated"] != true {
		t.Errorf("want authenticated=true, got %v", body["authenticated"])
	}
}

func TestAdminWhoami_NoCookieNoHeader_404(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	resp := doLoginReq(t, srv, http.MethodGet, "/admin/whoami", nil, nil)
	// adminTokenGate returns 404 on unauthorized (security-through-obscurity).
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestAdminProviders_CookieAuth_200(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	loginResp := doLoginReq(t, srv, http.MethodPost, "/admin/login", map[string]any{"token": loginTestToken}, nil)
	c := findCookie(loginResp, adminLoginCookieName)
	if c == nil {
		t.Fatal("no cookie from login")
	}

	// Cookie-authenticated request to a generic gated endpoint.
	resp := doLoginReq(t, srv, http.MethodGet, "/admin/providers", nil, []*http.Cookie{c})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie auth on /admin/providers: want 200, got %d", resp.StatusCode)
	}
}

func TestAdminProviders_HeaderAuth_200(t *testing.T) {
	srv := httptest.NewServer(buildLoginTestServer(loginTestToken))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/providers", nil)
	req.Header.Set("X-Relay-Admin-Token", loginTestToken)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("header auth on /admin/providers: want 200, got %d", resp.StatusCode)
	}
}

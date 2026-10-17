package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/oidc"
)

// startWithReturn is a start request to origin carrying return_to.
func startWithReturn(origin, returnTo string) *http.Request {
	return httptest.NewRequest("GET", origin+"/api/auth/oidc/start?return_to="+url.QueryEscape(returnTo), nil)
}

// startedFlow runs start as req, which must redirect to the IdP, and returns the issued state and flow cookie.
func (h *oidcHarness) startedFlow(req *http.Request) (string, *http.Cookie) {
	h.t.Helper()
	rec := h.do(req)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), h.idp.srv.URL) {
		h.t.Fatalf("start: status %d location %q body %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	h.idp.nonce = loc.Query().Get("nonce")
	flow := cookieNamed(rec, oidcFlowCookie)
	if flow == nil {
		h.t.Fatal("start set no flow cookie")
	}
	return loc.Query().Get("state"), flow
}

// signIn runs start as req, then the root callback with the IdP's code, reached the way start was: same host, peer, TLS and X-Forwarded-Proto.
func (h *oidcHarness) signIn(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	state, flow := h.startedFlow(req)
	return h.do(callbackLike(req, "code="+h.idp.issuedCode+"&state="+url.QueryEscape(state), flow))
}

func callbackLike(start *http.Request, query string, flow *http.Cookie) *http.Request {
	cb := httptest.NewRequest("GET", "http://"+start.Host+"/auth/callback?"+query, nil)
	cb.RemoteAddr, cb.TLS = start.RemoteAddr, start.TLS
	if p := start.Header.Get("X-Forwarded-Proto"); p != "" {
		cb.Header.Set("X-Forwarded-Proto", p)
	}
	cb.AddCookie(flow)
	return cb
}

func wantLocation(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
		t.Fatalf("callback: status %d location %q body %s, want 302 to %q", rec.Code, rec.Header().Get("Location"), rec.Body.String(), want)
	}
}

// wantErrorRedirect checks the callback sent the browser to returnTo with error=code added and nothing else changed.
func wantErrorRedirect(t *testing.T, rec *httptest.ResponseRecorder, returnTo, code string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d body %s, want a redirect", rec.Code, rec.Body.String())
	}
	got, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := url.Parse(returnTo)
	wantQuery := want.Query()
	wantQuery.Set("error", code)
	if got.Scheme != want.Scheme || got.Host != want.Host || got.Path != want.Path || got.Query().Encode() != wantQuery.Encode() {
		t.Fatalf("location = %q, want %q with error=%s", got, returnTo, code)
	}
}

// A return_to on the origin the browser reached start at round-trips through a successful sign-in.
func TestOIDCReturnTo_SameOrigin(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	const target = "http://control.test/ui/keys?tab=a"
	wantLocation(t, h.signIn(startWithReturn("http://control.test", target)), target)
}

// A return_to on a CORS-allowed origin is honoured, its host case and default port normalised.
func TestOIDCReturnTo_AllowedOrigin(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	h.od.allowOrigins = []string{"https://ui.example.com"}
	for _, target := range []string{"https://ui.example.com/welcome", "https://UI.Example.com:443/welcome"} {
		wantLocation(t, h.signIn(startWithReturn("http://control.test", target)), target)
	}
}

// Behind TLS-terminating ingress the request is plain http, but the configured callback names the public https origin.
func TestOIDCReturnTo_RedirectURLOrigin(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	h.cfg.RedirectURL = "https://relay.test/auth/callback"
	const target = "https://relay.test/ui"
	wantLocation(t, h.signIn(startWithReturn("http://relay.test", target)), target)
}

// PostLoginURL's origin is accepted when it is absolute.
func TestOIDCReturnTo_PostLoginURLOrigin(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	h.cfg.PostLoginURL = "https://ui.test/welcome"
	const target = "https://ui.test/keys"
	wantLocation(t, h.signIn(startWithReturn("http://control.test", target)), target)
}

// X-Forwarded-Proto counts only from a trusted proxy; TLS on the request itself always does.
func TestOIDCReturnTo_ForwardedProto(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	const target = "https://control.test/ui"
	spoofed := func() *http.Request {
		r := startWithReturn("http://control.test", target)
		r.Header.Set("X-Forwarded-Proto", "https")
		return r
	}
	rec := h.do(spoofed())
	wantOIDCError(t, rec, http.StatusBadRequest, "invalid_return_to", "return target not allowed")
	if cookieNamed(rec, oidcFlowCookie) != nil {
		t.Fatal("refused start set a flow cookie")
	}

	_, proxies, _ := net.ParseCIDR("192.0.2.0/24") // httptest's RemoteAddr
	h.od.trustedProxies = []*net.IPNet{proxies}
	wantLocation(t, h.signIn(spoofed()), target)

	h.od.trustedProxies = nil
	wantLocation(t, h.signIn(startWithReturn("https://control.test", target)), target)
}

// Anything but an absolute http(s) URL on an allowed origin is refused at start, before any flow begins.
func TestOIDCReturnTo_Refused(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	h.od.allowOrigins = []string{"https://ui.example.com"}
	for _, raw := range []string{
		"https://evil.test/ui",
		"https://control.test/ui",
		"http://ui.example.com/",
		"http://control.test:8080/",
		"http://control.test.evil.test/",
		"//evil.com",
		"//control.test/ui",
		"javascript:alert(1)",
		"/ui/keys",
		"ui/keys",
		"http://user@control.test/",
		"http://user:pw@control.test/",
		"http://@control.test/",
		"http:///ui",
		"http:control.test/ui",
		"ftp://control.test/",
		"http://control.test\\@evil.test/",
		"http://control.test/\n",
	} {
		rec := h.do(startWithReturn("http://control.test", raw))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("return_to %q: status %d location %q", raw, rec.Code, rec.Header().Get("Location"))
			continue
		}
		wantOIDCError(t, rec, http.StatusBadRequest, "invalid_return_to", "return target not allowed")
		if cookieNamed(rec, oidcFlowCookie) != nil {
			t.Errorf("return_to %q: refused start set a flow cookie", raw)
		}
	}
}

// The flow cookie is not signed: a target written into it is checked again and never followed when it fails.
func TestOIDCReturnTo_TamperedFlowCookie(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	start := startWithReturn("http://control.test", "http://control.test/ui")
	state, flow := h.startedFlow(start)
	raw, _ := base64.RawURLEncoding.DecodeString(flow.Value)
	var fs map[string]string
	if err := json.Unmarshal(raw, &fs); err != nil || fs["r"] != "http://control.test/ui" {
		t.Fatalf("flow cookie = %s", raw)
	}
	fs["r"] = "https://evil.test/"
	raw, _ = json.Marshal(fs)
	tampered := *flow
	tampered.Value = base64.RawURLEncoding.EncodeToString(raw)

	wantLocation(t, h.do(callbackLike(start, "code="+h.idp.issuedCode+"&state="+url.QueryEscape(state), &tampered)), "/")
	// A refused sign-in with the tampered cookie answers JSON instead of redirecting there.
	wantOIDCError(t, h.do(callbackLike(start, "code="+h.idp.issuedCode+"&state=forged", &tampered)), http.StatusBadRequest, "state_mismatch", "state mismatch")
}

// A refused sign-in with a return target goes back there with error=<code>, keeping the target's query, and puts no message or provider detail in the URL.
func TestOIDCReturnTo_DisabledAccountRedirects(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	_ = h.users.Upsert(context.Background(), &user.User{ID: "u1", Username: "alice", OIDCSubject: h.subject(), Disabled: true})
	const target = "http://control.test/login?next=%2Fkeys"
	rec := h.signIn(startWithReturn("http://control.test", target))
	wantErrorRedirect(t, rec, target, "account_disabled")
	if h.sess.calls != 0 {
		t.Fatal("session minted for a disabled account")
	}
	wantDenied(t, h.events(), "alice")
}

// The provider's own error goes back to the return target as idp_error, without the provider's text.
func TestOIDCReturnTo_ProviderErrorRedirects(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	const target = "http://control.test/login"
	start := startWithReturn("http://control.test", target)
	_, flow := h.startedFlow(start)
	rec := h.do(callbackLike(start, "error=access_denied&error_description=secret+detail", flow))
	wantErrorRedirect(t, rec, target, "idp_error")
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "access_denied") || strings.Contains(loc, "secret") {
		t.Fatalf("provider detail in the redirect: %q", loc)
	}
}

// Each refusal relay decides maps to its code and status: as the JSON envelope without a return target, as ?error=<code> with one.
func TestOIDCFailureCodes(t *testing.T) {
	cases := []struct {
		code   string
		status int
		setup  func(t *testing.T) *oidcHarness
	}{
		{"account_disabled", http.StatusUnauthorized, func(t *testing.T) *oidcHarness {
			h := newTestOIDC(t, newFakeIdP(t), "open")
			_ = h.users.Upsert(context.Background(), &user.User{ID: "u1", Username: "alice", OIDCSubject: h.subject(), Disabled: true})
			return h
		}},
		{"registration_closed", http.StatusForbidden, func(t *testing.T) *oidcHarness {
			return newTestOIDC(t, newFakeIdP(t), "closed")
		}},
		{"email_in_use", http.StatusForbidden, func(t *testing.T) *oidcHarness {
			h := newTestOIDC(t, newFakeIdP(t), "open")
			_ = h.users.Upsert(context.Background(), &user.User{ID: "u1", Username: "alice-scim", Email: h.idp.email})
			return h
		}},
		{"account_ambiguous", http.StatusConflict, func(t *testing.T) *oidcHarness {
			h := newLinkOIDC(t, "open")
			h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, "obj-1")
			h.users.provisioned(&user.User{ID: "u2", Username: "ada-okta"}, "obj-1")
			return h
		}},
		{"identity_conflict", http.StatusConflict, func(t *testing.T) *oidcHarness {
			h := newLinkOIDC(t, "closed")
			h.users.provisioned(&user.User{ID: "u1", Username: "ada"}, "obj-1")
			h.users.beforeLink = func() { h.users.byName["ada"].OIDCSubject = "https://other.example|x" }
			return h
		}},
		{"login_failed", http.StatusInternalServerError, func(t *testing.T) *oidcHarness {
			h := newTestOIDC(t, newFakeIdP(t), "open")
			h.sess.err = errors.New("kv down")
			return h
		}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			rec := tc.setup(t).signIn(httptest.NewRequest("GET", "http://control.test/api/auth/oidc/start", nil))
			var env struct{ Error struct{ Type, Code string } }
			if rec.Code != tc.status || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Error.Type != "authentication_error" || env.Error.Code != tc.code {
				t.Fatalf("without return_to: %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
		})
		t.Run(tc.code+" with return_to", func(t *testing.T) {
			const target = "http://control.test/login?next=%2Fkeys"
			wantErrorRedirect(t, tc.setup(t).signIn(startWithReturn("http://control.test", target)), target, tc.code)
		})
	}
}

// Without return_to the callback's failure is the JSON envelope, and a failure carrying no code is login_failed.
func TestRenderOIDCFailureDefaults(t *testing.T) {
	rec := httptest.NewRecorder()
	renderOIDCFailure(rec, httptest.NewRequest("GET", "/auth/callback", nil), &oidc.Failure{Status: http.StatusForbidden, Message: "no"})
	wantOIDCError(t, rec, http.StatusForbidden, "login_failed", "no")
}

package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// provider is a fake issuer: metadata at one discovery path and a token endpoint answering tokenBody for the code "good".
type provider struct {
	srv          *httptest.Server
	metadataPath string
	metadata     map[string]string
	tokenBody    map[string]any
	form         url.Values
	hits         atomic.Int32
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{metadataPath: "/.well-known/openid-configuration"}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case p.metadataPath:
			p.hits.Add(1)
			md := map[string]string{"authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token"}
			if p.metadata != nil {
				md = p.metadata
			}
			_ = json.NewEncoder(w).Encode(md)
		case "/token":
			_ = r.ParseForm()
			p.form = r.Form
			if r.Form.Get("code") != "good" {
				http.Error(w, "bad code", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p.tokenBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer", "id_token": jwt(`{"sub":"s1","email":"a@x.test","preferred_username":"al","sid":"sid1","groups":["g1",2,"g2"]}`)}
	return p
}

func jwt(payload string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

type recorder struct {
	provisioned []Claims
	loggedIn    []string
	denied      []*Failure
}

func newRP(p *provider, rec *recorder) *RelyingParty[string] {
	cfg := &Config{
		Issuer:       p.srv.URL,
		ClientID:     "c1",
		ClientSecret: "shh",
		RedirectURL:  "https://app.test/cb",
		Scopes:       []string{"openid", "email"},
		GroupsClaim:  "groups",
	}
	return &RelyingParty[string]{
		Config: func() *Config { return cfg },
		Provision: func(_ context.Context, c Claims) (string, error) {
			rec.provisioned = append(rec.provisioned, c)
			return "user-" + c.Subject, nil
		},
		Login: func(_ http.ResponseWriter, _ *http.Request, u string, _ Claims) error {
			rec.loggedIn = append(rec.loggedIn, u)
			return nil
		},
		OnDenied: func(_ *http.Request, f *Failure) { rec.denied = append(rec.denied, f) },
	}
}

func start(t *testing.T, rp *RelyingParty[string]) (url.Values, *http.Cookie) {
	t.Helper()
	return startAt(t, rp, "/start")
}

func startAt(t *testing.T, rp *RelyingParty[string], target string) (url.Values, *http.Cookie) {
	t.Helper()
	w := httptest.NewRecorder()
	rp.Start(w, httptest.NewRequest("GET", target, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("start set %d cookies", len(cookies))
	}
	return loc.Query(), cookies[0]
}

func callback(rp *RelyingParty[string], query string, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/cb?"+query, nil)
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	rp.Callback(w, r)
	return w
}

func login(t *testing.T, rp *RelyingParty[string], code string) *httptest.ResponseRecorder {
	t.Helper()
	q, c := start(t, rp)
	return callback(rp, "code="+code+"&state="+q.Get("state"), c)
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status || strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("got %d %q, want %d %q", w.Code, w.Body.String(), status, body)
	}
}

func TestStart(t *testing.T) {
	p := newProvider(t)
	rp := newRP(p, &recorder{})
	rp.Config().AuthParams = map[string]string{"prompt": "login"}
	q, c := start(t, rp)
	for k, v := range map[string]string{"client_id": "c1", "redirect_uri": "https://app.test/cb", "scope": "openid email", "response_type": "code", "code_challenge_method": "S256", "prompt": "login"} {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("state")) != 32 || q.Get("code_challenge") == "" || q.Has("nonce") {
		t.Errorf("authorize query = %v", q)
	}
	if c.Name != "oidc_flow" || c.Path != "/" || c.MaxAge != 600 || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Errorf("flow cookie = %+v", c)
	}

	rp.FlowCookie, rp.CookieSecure = "app_flow", true
	if _, c := start(t, rp); c.Name != "app_flow" || !c.Secure {
		t.Errorf("flow cookie = %+v", c)
	}
}

func TestDisabled(t *testing.T) {
	rp := &RelyingParty[string]{Config: func() *Config { return nil }}
	for _, h := range []http.HandlerFunc{rp.Start, rp.Callback} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/", nil))
		wantStatus(t, w, http.StatusNotFound, "oidc login is not enabled")
	}
}

func TestCallbackSuccess(t *testing.T) {
	p := newProvider(t)
	rec := &recorder{}
	rp := newRP(p, rec)
	w := login(t, rp, "good")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != "oidc_flow" || c[0].MaxAge != -1 {
		t.Errorf("flow cookie not cleared: %+v", c)
	}
	want := Claims{Issuer: p.srv.URL, Subject: "s1", Email: "a@x.test", PreferredUsername: "al", SessionID: "sid1", Groups: []string{"g1", "g2"}}
	if len(rec.provisioned) != 1 || !equalClaims(rec.provisioned[0], want) {
		t.Fatalf("claims = %+v, want %+v", rec.provisioned, want)
	}
	if len(rec.loggedIn) != 1 || rec.loggedIn[0] != "user-s1" || len(rec.denied) != 0 {
		t.Fatalf("recorder = %+v", rec)
	}
	if p.form.Get("client_secret") != "shh" || p.form.Get("client_id") != "c1" || p.form.Get("code_verifier") == "" || p.form.Get("redirect_uri") != "https://app.test/cb" {
		t.Errorf("token request form = %v", p.form)
	}

	rp.Config().PostLoginURL = "https://ui.test/"
	if w := login(t, rp, "good"); w.Header().Get("Location") != "https://ui.test/" {
		t.Fatalf("location = %q", w.Header().Get("Location"))
	}
}

func TestSafeReturnPath(t *testing.T) {
	accepted := []string{"/", "/ui/keys", "/ui/keys?tab=a&x=1#top", "/a%20b", "/a/../b"}
	for _, s := range accepted {
		if !safeReturnPath(s) {
			t.Errorf("rejected %q", s)
		}
	}
	rejected := []string{
		"", "ui/keys", "//evil.com", "///evil.com", "/\\evil.com", "\\evil.com", "/ui\\x",
		"https://evil.com", "http:/evil.com", "javascript:alert(1)", "data:text/html,x",
		"/%2Fevil.com", "/%2F/evil.com", "/%5Cevil.com", "/%zz", "/a%0d%0aSet-Cookie:x=y",
		"/a\tb", "/a\nb", "/a\x7f", "/a\u0085", "/a\xff", "/" + strings.Repeat("a", maxReturnLen),
	}
	for _, s := range rejected {
		if safeReturnPath(s) {
			t.Errorf("accepted %q", s)
		}
	}
}

func flowOf(t *testing.T, c *http.Cookie) flowState {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatal(err)
	}
	var fs flowState
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestReturnPath(t *testing.T) {
	p := newProvider(t)
	rp := newRP(p, &recorder{})
	rp.Config().PostLoginURL = "https://ui.test/"
	loginFrom := func(target string) *httptest.ResponseRecorder {
		q, c := startAt(t, rp, target)
		return callback(rp, "code=good&state="+q.Get("state"), c)
	}

	if w := loginFrom("/start?rd=" + url.QueryEscape("/ui/keys?tab=a")); w.Code != http.StatusFound || w.Header().Get("Location") != "/ui/keys?tab=a" {
		t.Fatalf("accepted rd: %d %q", w.Code, w.Header().Get("Location"))
	}

	for _, rd := range []string{"//evil.com", "/\\evil.com", "https://evil.com", "javascript:alert(1)", "/%2F%2Fevil.com", "%2F%2Fevil.com"} {
		target := "/start?rd=" + url.QueryEscape(rd)
		if _, c := startAt(t, rp, target); flowOf(t, c).Return != "" {
			t.Errorf("rd %q carried in the flow cookie", rd)
		}
		if w := loginFrom(target); w.Header().Get("Location") != "https://ui.test/" {
			t.Errorf("rd %q: location %q, want PostLoginURL", rd, w.Header().Get("Location"))
		}
	}
	// Already-decoded by the query parser: rd=%2F%2Fevil.com arrives as //evil.com.
	if w := loginFrom("/start?rd=%2F%2Fevil.com"); w.Header().Get("Location") != "https://ui.test/" {
		t.Errorf("encoded rd: location %q", w.Header().Get("Location"))
	}

	// Without rd the flow cookie has the payload it always had.
	_, c := start(t, rp)
	raw, _ := base64.RawURLEncoding.DecodeString(c.Value)
	if strings.Contains(string(raw), `"r"`) {
		t.Errorf("flow cookie without rd = %s", raw)
	}

	rp.ReturnParam = "next"
	if w := loginFrom("/start?rd=/ignored&next=/ui/next"); w.Header().Get("Location") != "/ui/next" {
		t.Errorf("renamed param: location %q", w.Header().Get("Location"))
	}
}

// The flow cookie is not signed: a return path planted in it is checked again at the callback.
func TestReturnPathTamperedCookie(t *testing.T) {
	p := newProvider(t)
	rp := newRP(p, &recorder{})
	q, c := start(t, rp)
	fs := flowOf(t, c)
	fs.Return = "//evil.com"
	raw, _ := json.Marshal(fs)
	c.Value = base64.RawURLEncoding.EncodeToString(raw)
	if w := callback(rp, "code=good&state="+q.Get("state"), c); w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("tampered cookie: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func equalClaims(a, b Claims) bool {
	return a.Issuer == b.Issuer && a.Subject == b.Subject && a.Email == b.Email && a.PreferredUsername == b.PreferredUsername &&
		a.SessionID == b.SessionID && strings.Join(a.Groups, ",") == strings.Join(b.Groups, ",")
}

func TestCallbackRefusals(t *testing.T) {
	p := newProvider(t)
	rec := &recorder{}
	rp := newRP(p, rec)

	w := callback(rp, "error=access_denied", nil)
	wantStatus(t, w, http.StatusBadGateway, "login failed at identity provider: access_denied")

	wantStatus(t, callback(rp, "code=good&state=x", nil), http.StatusBadRequest, "login flow expired — start again")
	for _, v := range []string{"", "%%%", base64.RawURLEncoding.EncodeToString([]byte("{")), base64.RawURLEncoding.EncodeToString([]byte(`{"s":"x"}`))} {
		wantStatus(t, callback(rp, "code=good&state=x", &http.Cookie{Name: "oidc_flow", Value: v}), http.StatusBadRequest, "login flow expired — start again")
	}

	q, c := start(t, rp)
	wantStatus(t, callback(rp, "code=good&state=forged", c), http.StatusBadRequest, "state mismatch")
	wantStatus(t, callback(rp, "code=good", c), http.StatusBadRequest, "state mismatch")
	wantStatus(t, callback(rp, "state="+q.Get("state"), c), http.StatusBadRequest, "missing code")
	wantStatus(t, login(t, rp, "bad"), http.StatusBadGateway, "code exchange failed")

	if len(rec.denied) != 10 || len(rec.provisioned) != 0 {
		t.Fatalf("denied %d, provisioned %d", len(rec.denied), len(rec.provisioned))
	}
	for _, f := range rec.denied {
		if f.Name != "" {
			t.Errorf("refusal before the token names %q", f.Name)
		}
	}
}

func TestCallbackDiscoveryFailureIsNotDenied(t *testing.T) {
	p := newProvider(t)
	rec := &recorder{}
	rp := newRP(p, rec)
	q, c := start(t, rp)
	rp.Config().Issuer = p.srv.URL + "/gone"
	wantStatus(t, callback(rp, "code=good&state="+q.Get("state"), c), http.StatusBadGateway, "identity provider unavailable")
	if len(rec.denied) != 0 {
		t.Fatalf("discovery failure reported as denied: %+v", rec.denied)
	}

	w := httptest.NewRecorder()
	rp.Start(w, httptest.NewRequest("GET", "/start", nil))
	wantStatus(t, w, http.StatusBadGateway, "identity provider unavailable")
}

func TestCallbackUnusableToken(t *testing.T) {
	cases := map[string]map[string]any{
		"not a jwt":      {"id_token": "abc"},
		"bad base64":     {"id_token": "h.%%%.s"},
		"bad json":       {"id_token": jwt(`[`)},
		"no sub":         {"id_token": jwt(`{"email":"a@x.test"}`)},
		"neither":        {},
		"profile no id":  {"user": map[string]any{"email": "a@x.test"}},
		"profile not ok": {"user": "s1"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t)
			p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer"}
			for k, v := range extra {
				p.tokenBody[k] = v
			}
			rec := &recorder{}
			wantStatus(t, login(t, newRP(p, rec), "good"), http.StatusBadGateway, "identity provider returned no usable identity")
			if len(rec.denied) != 1 || rec.denied[0].Err == nil {
				t.Fatalf("denied = %+v", rec.denied)
			}
		})
	}
}

func TestCallbackInlineProfile(t *testing.T) {
	p := newProvider(t)
	p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer", "user": map[string]any{"id": "u9", "email": "b@x.test"}}
	rec := &recorder{}
	if w := login(t, newRP(p, rec), "good"); w.Code != http.StatusFound {
		t.Fatalf("status %d", w.Code)
	}
	c := rec.provisioned[0]
	if c.Subject != "u9" || c.Email != "b@x.test" || c.SessionID != "" || c.Groups == nil || len(c.Groups) != 0 {
		t.Fatalf("claims = %+v", c)
	}
}

func TestCallbackHookFailures(t *testing.T) {
	refused := &Failure{Status: http.StatusForbidden, Message: "closed", Name: "who"}
	cases := []struct {
		name                       string
		provision, login           error
		status                     int
		body, deniedAs             string
		loginCalls, provisionCalls int
	}{
		{"provision failure", refused, nil, http.StatusForbidden, "closed", "who", 0, 1},
		{"provision error", errors.New("db down"), nil, http.StatusInternalServerError, "account provisioning failed", "s1", 0, 1},
		{"login failure", nil, refused, http.StatusForbidden, "closed", "who", 1, 1},
		{"login error", nil, errors.New("kv down"), http.StatusInternalServerError, "login failed", "s1", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t)
			rec := &recorder{}
			rp := newRP(p, rec)
			provision, loginHook := rp.Provision, rp.Login
			rp.Provision = func(ctx context.Context, c Claims) (string, error) {
				u, _ := provision(ctx, c)
				return u, tc.provision
			}
			rp.Login = func(w http.ResponseWriter, r *http.Request, u string, c Claims) error {
				_ = loginHook(w, r, u, c)
				return tc.login
			}
			wantStatus(t, login(t, rp, "good"), tc.status, tc.body)
			if len(rec.denied) != 1 || rec.denied[0].Name != tc.deniedAs || len(rec.loggedIn) != tc.loginCalls || len(rec.provisioned) != tc.provisionCalls {
				t.Fatalf("recorder = %+v", rec)
			}
		})
	}
}

func TestDiscovery(t *testing.T) {
	t.Run("cached per issuer, client and redirect", func(t *testing.T) {
		p := newProvider(t)
		rp := newRP(p, &recorder{})
		for range 3 {
			start(t, rp)
		}
		if p.hits.Load() != 1 {
			t.Fatalf("metadata fetched %d times", p.hits.Load())
		}
		rp.Config().RedirectURL = "https://app.test/other"
		start(t, rp)
		if p.hits.Load() != 2 {
			t.Fatalf("metadata fetched %d times after a redirect change", p.hits.Load())
		}
		for k, e := range rp.discovery {
			e.expires = time.Now().Add(-time.Second)
			rp.discovery[k] = e
		}
		start(t, rp)
		if p.hits.Load() != 3 {
			t.Fatalf("expired entry not refetched: %d", p.hits.Load())
		}
	})
	t.Run("RFC 8414 path first", func(t *testing.T) {
		p := newProvider(t)
		p.metadataPath = "/.well-known/oauth-authorization-server"
		md, err := discover(context.Background(), p.srv.URL+"/")
		if err != nil || md.TokenEndpoint != p.srv.URL+"/token" {
			t.Fatalf("discover = %+v, %v", md, err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		p := newProvider(t)
		p.metadata = map[string]string{"token_endpoint": "x"}
		if _, err := discover(context.Background(), p.srv.URL); err == nil || !strings.Contains(err.Error(), "authorization_endpoint") {
			t.Errorf("incomplete metadata: %v", err)
		}
		if _, err := discover(context.Background(), ""); err == nil {
			t.Error("empty issuer accepted")
		}
		if _, err := discover(context.Background(), "http://127.0.0.1:0"); err == nil {
			t.Error("unreachable issuer accepted")
		}
		if _, err := discover(context.Background(), "http://bad host"); err == nil {
			t.Error("malformed issuer accepted")
		}
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
		defer bad.Close()
		if _, err := discover(context.Background(), bad.URL); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Errorf("non-JSON metadata: %v", err)
		}
	})
}

func TestClaimsFromToken(t *testing.T) {
	tok := (&oauth2.Token{}).WithExtra(map[string]any{"id_token": jwt(`{"sub":"x","roles":["a"],"groups":"g"}`)})
	c, err := claimsFromToken(tok, "iss", "roles")
	if err != nil || c.Subject != "x" || strings.Join(c.Groups, ",") != "a" {
		t.Fatalf("claims = %+v, %v", c, err)
	}
	if c, _ := claimsFromToken(tok, "iss", "groups"); len(c.Groups) != 0 {
		t.Errorf("non-array groups read: %v", c.Groups)
	}
	if c, _ := claimsFromToken(tok, "iss", ""); c.Groups != nil {
		t.Errorf("no claim configured, groups = %v", c.Groups)
	}
}

func TestFailureError(t *testing.T) {
	cause := errors.New("cause")
	f := &Failure{Message: "m", Err: cause}
	if f.Error() != "m: cause" || !errors.Is(f, cause) {
		t.Errorf("Error = %q", f.Error())
	}
	if (&Failure{Message: "m"}).Error() != "m" {
		t.Error("Error without cause")
	}
}

package oidc

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testKeys are generated once: RSA key generation is slow enough to matter across many fake providers.
var testKeys = sync.OnceValue(func() []*rsa.PrivateKey {
	keys := make([]*rsa.PrivateKey, 3)
	for i := range keys {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keys[i] = k
	}
	return keys
})

// provider is a fake issuer: OpenID metadata, a JWKS, and a token endpoint answering the code "good" with an RS256 id_token carrying the nonce of the last Start.
type provider struct {
	srv      *httptest.Server
	metadata map[string]any
	// published is the JWKS by kid.
	published map[string]*rsa.PrivateKey
	// kid and signKey sign the id_token; signKey defaults to published[kid].
	kid     string
	signKey *rsa.PrivateKey
	// alg is the id_token's header alg: RS256 (default), HS256 with the client secret, or none.
	alg string
	// claims override or add id_token claims; a nil value removes one.
	claims map[string]any
	nonce  string
	// tokenBody, when set, is the token response verbatim.
	tokenBody map[string]any
	form      url.Values
	hits      atomic.Int32
	jwksHits  atomic.Int32
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	keys := testKeys()
	p := &provider{published: map[string]*rsa.PrivateKey{"k1": keys[0]}, kid: "k1", alg: "RS256"}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			p.hits.Add(1)
			md := map[string]any{
				"issuer":                 p.srv.URL,
				"authorization_endpoint": p.srv.URL + "/authorize",
				"token_endpoint":         p.srv.URL + "/token",
				"jwks_uri":               p.srv.URL + "/jwks",
			}
			if p.metadata != nil {
				md = p.metadata
			}
			_ = json.NewEncoder(w).Encode(md)
		case "/jwks":
			p.jwksHits.Add(1)
			var keys []map[string]string
			for kid, k := range p.published {
				keys = append(keys, map[string]string{
					"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
					"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
					"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		case "/token":
			_ = r.ParseForm()
			p.form = r.Form
			if r.Form.Get("code") != "good" {
				http.Error(w, "bad code", http.StatusBadRequest)
				return
			}
			body := p.tokenBody
			if body == nil {
				body = map[string]any{"access_token": "at", "token_type": "bearer", "id_token": p.idToken()}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *provider) idToken() string {
	now := time.Now()
	claims := map[string]any{
		"iss": p.srv.URL, "aud": "c1", "sub": "s1", "email": "a@x.test", "preferred_username": "al", "sid": "sid1",
		"groups": []any{"g1", 2, "g2"}, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": p.nonce,
	}
	for k, v := range p.claims {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	header, _ := json.Marshal(map[string]string{"alg": p.alg, "kid": p.kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	var sig []byte
	switch p.alg {
	case "none":
	case "HS256":
		mac := hmac.New(sha256.New, []byte("shh"))
		mac.Write([]byte(input))
		sig = mac.Sum(nil)
	default:
		key := p.signKey
		if key == nil {
			key = p.published[p.kid]
		}
		digest := sha256.Sum256([]byte(input))
		sig, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type recorder struct {
	provisioned []Claims
	loggedIn    []string
	denied      []*Failure
}

// fixture is a relying party wired to a fake provider.
type fixture struct {
	t   *testing.T
	p   *provider
	rp  *RelyingParty[string]
	rec *recorder
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	p := newProvider(t)
	rec := &recorder{}
	cfg := &Config{
		Issuer:       p.srv.URL,
		ClientID:     "c1",
		ClientSecret: "shh",
		RedirectURL:  "https://app.test/cb",
		Scopes:       []string{"openid", "email"},
		GroupsClaim:  "groups",
	}
	rp := &RelyingParty[string]{
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
	return &fixture{t: t, p: p, rp: rp, rec: rec}
}

func (f *fixture) start() (url.Values, *http.Cookie) {
	f.t.Helper()
	return f.startAt("/start")
}

// startAt runs Start and hands the authorize request's nonce to the provider, as a real one would carry it into the id_token.
func (f *fixture) startAt(target string) (url.Values, *http.Cookie) {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.rp.Start(w, httptest.NewRequest("GET", target, nil))
	if w.Code != http.StatusFound {
		f.t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		f.t.Fatalf("start set %d cookies", len(cookies))
	}
	f.p.nonce = loc.Query().Get("nonce")
	return loc.Query(), cookies[0]
}

func (f *fixture) callback(query string, c *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/cb?"+query, nil)
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.rp.Callback(w, r)
	return w
}

func (f *fixture) login(code string) *httptest.ResponseRecorder {
	f.t.Helper()
	q, c := f.start()
	return f.callback("code="+code+"&state="+q.Get("state"), c)
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status || strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("got %d %q, want %d %q", w.Code, w.Body.String(), status, body)
	}
}

func TestStart(t *testing.T) {
	f := newFixture(t)
	f.rp.Config().AuthParams = map[string]string{"prompt": "login"}
	q, c := f.start()
	for k, v := range map[string]string{"client_id": "c1", "redirect_uri": "https://app.test/cb", "scope": "openid email", "response_type": "code", "code_challenge_method": "S256", "prompt": "login"} {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("state")) != 32 || q.Get("code_challenge") == "" || len(q.Get("nonce")) != 32 || q.Get("nonce") == q.Get("state") {
		t.Errorf("authorize query = %v", q)
	}
	if fs := flowOf(t, c); fs.Nonce != q.Get("nonce") || fs.State != q.Get("state") {
		t.Errorf("flow cookie = %+v", fs)
	}
	if c.Name != "oidc_flow" || c.Path != "/" || c.MaxAge != 600 || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Errorf("flow cookie = %+v", c)
	}

	f.rp.FlowCookie, f.rp.CookieSecure = "app_flow", true
	if _, c := f.start(); c.Name != "app_flow" || !c.Secure {
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
	f := newFixture(t)
	w := f.login("good")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != "oidc_flow" || c[0].MaxAge != -1 {
		t.Errorf("flow cookie not cleared: %+v", c)
	}
	want := Claims{Issuer: f.p.srv.URL, Subject: "s1", Email: "a@x.test", PreferredUsername: "al", SessionID: "sid1", Groups: []string{"g1", "g2"}}
	if len(f.rec.provisioned) != 1 || !equalClaims(f.rec.provisioned[0], want) {
		t.Fatalf("claims = %+v, want %+v", f.rec.provisioned, want)
	}
	if len(f.rec.loggedIn) != 1 || f.rec.loggedIn[0] != "user-s1" || len(f.rec.denied) != 0 {
		t.Fatalf("recorder = %+v", f.rec)
	}
	if form := f.p.form; form.Get("client_secret") != "shh" || form.Get("client_id") != "c1" || form.Get("code_verifier") == "" || form.Get("redirect_uri") != "https://app.test/cb" {
		t.Errorf("token request form = %v", form)
	}

	f.rp.Config().PostLoginURL = "https://ui.test/"
	if w := f.login("good"); w.Header().Get("Location") != "https://ui.test/" {
		t.Fatalf("location = %q", w.Header().Get("Location"))
	}
	if n := f.p.jwksHits.Load(); n != 1 {
		t.Errorf("JWKS fetched %d times over two logins, want 1", n)
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

func withFlow(c *http.Cookie, fs flowState) *http.Cookie {
	raw, _ := json.Marshal(fs)
	out := *c
	out.Value = base64.RawURLEncoding.EncodeToString(raw)
	return &out
}

func TestReturnPath(t *testing.T) {
	f := newFixture(t)
	f.rp.Config().PostLoginURL = "https://ui.test/"
	loginFrom := func(target string) *httptest.ResponseRecorder {
		q, c := f.startAt(target)
		return f.callback("code=good&state="+q.Get("state"), c)
	}

	if w := loginFrom("/start?rd=" + url.QueryEscape("/ui/keys?tab=a")); w.Code != http.StatusFound || w.Header().Get("Location") != "/ui/keys?tab=a" {
		t.Fatalf("accepted rd: %d %q", w.Code, w.Header().Get("Location"))
	}

	for _, rd := range []string{"//evil.com", "/\\evil.com", "https://evil.com", "javascript:alert(1)", "/%2F%2Fevil.com", "%2F%2Fevil.com"} {
		target := "/start?rd=" + url.QueryEscape(rd)
		if _, c := f.startAt(target); flowOf(t, c).Return != "" {
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

	// Without rd the flow cookie carries no return path.
	_, c := f.start()
	raw, _ := base64.RawURLEncoding.DecodeString(c.Value)
	if strings.Contains(string(raw), `"r"`) {
		t.Errorf("flow cookie without rd = %s", raw)
	}

	f.rp.ReturnParam = "next"
	if w := loginFrom("/start?rd=/ignored&next=/ui/next"); w.Header().Get("Location") != "/ui/next" {
		t.Errorf("renamed param: location %q", w.Header().Get("Location"))
	}
}

// The flow cookie is not signed: a return path planted in it is checked again at the callback.
func TestReturnPathTamperedCookie(t *testing.T) {
	f := newFixture(t)
	q, c := f.start()
	fs := flowOf(t, c)
	fs.Return = "//evil.com"
	if w := f.callback("code=good&state="+q.Get("state"), withFlow(c, fs)); w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("tampered cookie: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func equalClaims(a, b Claims) bool {
	return a.Issuer == b.Issuer && a.Subject == b.Subject && a.Email == b.Email && a.PreferredUsername == b.PreferredUsername &&
		a.SessionID == b.SessionID && strings.Join(a.Groups, ",") == strings.Join(b.Groups, ",")
}

func TestCallbackRefusals(t *testing.T) {
	f := newFixture(t)

	w := f.callback("error=access_denied", nil)
	wantStatus(t, w, http.StatusBadGateway, "login failed at identity provider: access_denied")

	wantStatus(t, f.callback("code=good&state=x", nil), http.StatusBadRequest, "login flow expired — start again")
	for _, v := range []string{"", "%%%", base64.RawURLEncoding.EncodeToString([]byte("{")), base64.RawURLEncoding.EncodeToString([]byte(`{"s":"x"}`))} {
		wantStatus(t, f.callback("code=good&state=x", &http.Cookie{Name: "oidc_flow", Value: v}), http.StatusBadRequest, "login flow expired — start again")
	}

	q, c := f.start()
	wantStatus(t, f.callback("code=good&state=forged", c), http.StatusBadRequest, "state mismatch")
	wantStatus(t, f.callback("code=good", c), http.StatusBadRequest, "state mismatch")
	wantStatus(t, f.callback("state="+q.Get("state"), c), http.StatusBadRequest, "missing code")
	wantStatus(t, f.login("bad"), http.StatusBadGateway, "code exchange failed")

	if len(f.rec.denied) != 10 || len(f.rec.provisioned) != 0 {
		t.Fatalf("denied %d, provisioned %d", len(f.rec.denied), len(f.rec.provisioned))
	}
	for _, d := range f.rec.denied {
		if d.Name != "" {
			t.Errorf("refusal before the token names %q", d.Name)
		}
	}
}

// A flow cookie set before nonces existed carries none; that login is refused as expired so the browser starts it over.
func TestCallbackFlowCookieWithoutNonce(t *testing.T) {
	f := newFixture(t)
	q, c := f.start()
	fs := flowOf(t, c)
	fs.Nonce = ""
	wantStatus(t, f.callback("code=good&state="+q.Get("state"), withFlow(c, fs)), http.StatusBadRequest, "login flow expired — start again")
	if len(f.rec.provisioned) != 0 {
		t.Fatal("login without a nonce went through")
	}
	if w := f.login("good"); w.Code != http.StatusFound {
		t.Fatalf("restarted login: %d %s", w.Code, w.Body.String())
	}
}

func TestCallbackDiscoveryFailureIsNotDenied(t *testing.T) {
	f := newFixture(t)
	q, c := f.start()
	f.rp.Config().Issuer = f.p.srv.URL + "/gone"
	wantStatus(t, f.callback("code=good&state="+q.Get("state"), c), http.StatusBadGateway, "identity provider unavailable")
	if len(f.rec.denied) != 0 {
		t.Fatalf("discovery failure reported as denied: %+v", f.rec.denied)
	}

	w := httptest.NewRecorder()
	f.rp.Start(w, httptest.NewRequest("GET", "/start", nil))
	wantStatus(t, w, http.StatusBadGateway, "identity provider unavailable")
}

// Each id_token that fails OIDC Core 3.1.3.7 validation is refused with the reason in the Failure's error.
func TestCallbackRejectsIDToken(t *testing.T) {
	keys := testKeys()
	now := time.Now()
	cases := []struct {
		name   string
		mutate func(*provider)
		reason string
	}{
		{"bad signature", func(p *provider) { p.signKey = keys[1] }, "failed to verify signature"},
		{"unknown kid", func(p *provider) { p.kid, p.signKey = "k9", keys[2] }, "failed to verify signature"},
		{"alg none", func(p *provider) { p.alg = "none" }, "malformed jwt"},
		{"HS256 with the client secret", func(p *provider) { p.alg = "HS256" }, "malformed jwt"},
		{"wrong iss", func(p *provider) { p.claims = map[string]any{"iss": "https://evil.example"} }, "issued by a different provider"},
		{"wrong aud", func(p *provider) { p.claims = map[string]any{"aud": "someone-else"} }, "expected audience"},
		{"several aud without azp", func(p *provider) { p.claims = map[string]any{"aud": []string{"c1", "other"}} }, `azp "" is not the client id`},
		{"azp of another client", func(p *provider) { p.claims = map[string]any{"aud": []string{"c1", "other"}, "azp": "other"} }, `azp "other" is not the client id`},
		{"expired", func(p *provider) { p.claims = map[string]any{"exp": now.Add(-time.Minute).Unix()} }, "token is expired"},
		{"no exp", func(p *provider) { p.claims = map[string]any{"exp": nil} }, "token is expired"},
		{"iat in the future", func(p *provider) { p.claims = map[string]any{"iat": now.Add(time.Hour).Unix()} }, "is in the future"},
		{"missing nonce", func(p *provider) { p.claims = map[string]any{"nonce": nil} }, "nonce missing"},
		{"wrong nonce", func(p *provider) { p.claims = map[string]any{"nonce": "attacker"} }, "nonce mismatch"},
		{"not a jwt", func(p *provider) {
			p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer", "id_token": "abc"}
		}, "malformed jwt"},
		{"present beside an inline profile", func(p *provider) {
			p.signKey = keys[1]
			p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer", "id_token": p.idToken(), "user": map[string]any{"id": "u9"}}
		}, "failed to verify signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if w := f.login("good"); w.Code != http.StatusFound {
				t.Fatalf("warm-up login: %d %s", w.Code, w.Body.String())
			}
			tc.mutate(f.p)
			wantStatus(t, f.login("good"), http.StatusBadGateway, "identity provider returned an invalid id_token")
			if len(f.rec.provisioned) != 1 || len(f.rec.denied) != 1 {
				t.Fatalf("recorder = %+v", f.rec)
			}
			if err := f.rec.denied[0].Err; !errors.Is(err, errInvalidIDToken) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error = %v, want %q", err, tc.reason)
			}
		})
	}
}

// An unknown kid refetches the JWKS once before failing; a kid published since the last fetch is found by that refetch.
func TestCallbackKeyRefetch(t *testing.T) {
	keys := testKeys()
	t.Run("unknown kid refetches then fails", func(t *testing.T) {
		f := newFixture(t)
		f.login("good")
		f.p.kid, f.p.signKey = "k9", keys[2]
		wantStatus(t, f.login("good"), http.StatusBadGateway, "identity provider returned an invalid id_token")
		if n := f.p.jwksHits.Load(); n != 2 {
			t.Fatalf("JWKS fetched %d times, want 2", n)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		f := newFixture(t)
		f.login("good")
		f.p.published = map[string]*rsa.PrivateKey{"k1": keys[0], "k2": keys[1]}
		f.p.kid = "k2"
		if w := f.login("good"); w.Code != http.StatusFound {
			t.Fatalf("rotated key: %d %s", w.Code, w.Body.String())
		}
		if n := f.p.jwksHits.Load(); n != 2 || len(f.rec.loggedIn) != 2 {
			t.Fatalf("JWKS fetched %d times, logged in %d", n, len(f.rec.loggedIn))
		}
	})
}

func TestCallbackAcceptsIDToken(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   Claims
	}{
		// Zitadel without idTokenUserinfoAssertion: only the protocol claims and sub.
		{"sub only", map[string]any{"email": nil, "preferred_username": nil, "sid": nil, "groups": nil}, Claims{Subject: "s1"}},
		// Zitadel's audience lists the client and its project, with azp naming the client.
		{"several aud with azp", map[string]any{"aud": []string{"c1", "project"}, "azp": "c1"}, Claims{Subject: "s1", Email: "a@x.test", PreferredUsername: "al", SessionID: "sid1", Groups: []string{"g1", "g2"}}},
		{"iat within skew", map[string]any{"iat": time.Now().Add(time.Minute).Unix()}, Claims{Subject: "s1", Email: "a@x.test", PreferredUsername: "al", SessionID: "sid1", Groups: []string{"g1", "g2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.p.claims = tc.claims
			if w := f.login("good"); w.Code != http.StatusFound {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			tc.want.Issuer = f.p.srv.URL
			if got := f.rec.provisioned[0]; !equalClaims(got, tc.want) {
				t.Fatalf("claims = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCallbackUnusableToken(t *testing.T) {
	cases := map[string]func(*provider){
		"no sub":  func(p *provider) { p.claims = map[string]any{"sub": nil} },
		"neither": func(p *provider) { p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer"} },
		"profile no id": func(p *provider) {
			p.tokenBody = map[string]any{"access_token": "at", "user": map[string]any{"email": "a@x.test"}}
		},
		"profile not ok": func(p *provider) { p.tokenBody = map[string]any{"access_token": "at", "user": "s1"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f.p)
			wantStatus(t, f.login("good"), http.StatusBadGateway, "identity provider returned no usable identity")
			if len(f.rec.denied) != 1 || f.rec.denied[0].Err == nil || errors.Is(f.rec.denied[0].Err, errInvalidIDToken) {
				t.Fatalf("denied = %+v", f.rec.denied)
			}
		})
	}
}

// Without an id_token the inline profile is trusted on the code exchange alone, and no JWKS is fetched.
func TestCallbackInlineProfile(t *testing.T) {
	f := newFixture(t)
	f.p.tokenBody = map[string]any{"access_token": "at", "token_type": "bearer", "user": map[string]any{"id": "u9", "email": "b@x.test"}}
	if w := f.login("good"); w.Code != http.StatusFound {
		t.Fatalf("status %d", w.Code)
	}
	c := f.rec.provisioned[0]
	if c.Subject != "u9" || c.Email != "b@x.test" || c.SessionID != "" || c.Groups == nil || len(c.Groups) != 0 {
		t.Fatalf("claims = %+v", c)
	}
	if n := f.p.jwksHits.Load(); n != 0 {
		t.Errorf("JWKS fetched %d times", n)
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
			f := newFixture(t)
			provision, loginHook := f.rp.Provision, f.rp.Login
			f.rp.Provision = func(ctx context.Context, c Claims) (string, error) {
				u, _ := provision(ctx, c)
				return u, tc.provision
			}
			f.rp.Login = func(w http.ResponseWriter, r *http.Request, u string, c Claims) error {
				_ = loginHook(w, r, u, c)
				return tc.login
			}
			wantStatus(t, f.login("good"), tc.status, tc.body)
			if rec := f.rec; len(rec.denied) != 1 || rec.denied[0].Name != tc.deniedAs || len(rec.loggedIn) != tc.loginCalls || len(rec.provisioned) != tc.provisionCalls {
				t.Fatalf("recorder = %+v", rec)
			}
		})
	}
}

func TestDiscovery(t *testing.T) {
	t.Run("cached per issuer, client and redirect", func(t *testing.T) {
		f := newFixture(t)
		for range 3 {
			f.start()
		}
		if f.p.hits.Load() != 1 {
			t.Fatalf("metadata fetched %d times", f.p.hits.Load())
		}
		f.rp.Config().RedirectURL = "https://app.test/other"
		f.start()
		if f.p.hits.Load() != 2 {
			t.Fatalf("metadata fetched %d times after a redirect change", f.p.hits.Load())
		}
		for k, e := range f.rp.discovery {
			e.expires = time.Now().Add(-time.Second)
			f.rp.discovery[k] = e
		}
		f.start()
		if f.p.hits.Load() != 3 {
			t.Fatalf("expired entry not refetched: %d", f.p.hits.Load())
		}
	})
	t.Run("issuer mismatch", func(t *testing.T) {
		f := newFixture(t)
		f.p.metadata = map[string]any{"issuer": "https://evil.example", "authorization_endpoint": "x", "token_endpoint": "y"}
		if _, err := discover(context.Background(), f.p.srv.URL); err == nil || !strings.Contains(err.Error(), "did not match the issuer") {
			t.Fatalf("mismatched issuer: %v", err)
		}
		w := httptest.NewRecorder()
		f.rp.Start(w, httptest.NewRequest("GET", "/start", nil))
		wantStatus(t, w, http.StatusBadGateway, "identity provider unavailable")
		// The configured issuer with a trailing slash the provider does not publish is a mismatch too.
		f.p.metadata = nil
		if _, err := discover(context.Background(), f.p.srv.URL+"/"); err == nil {
			t.Fatal("trailing-slash issuer accepted")
		}
	})
	t.Run("errors", func(t *testing.T) {
		f := newFixture(t)
		f.p.metadata = map[string]any{"issuer": f.p.srv.URL, "token_endpoint": "x"}
		if _, err := discover(context.Background(), f.p.srv.URL); err == nil || !strings.Contains(err.Error(), "authorization_endpoint") {
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

func TestGroupsClaim(t *testing.T) {
	f := newFixture(t)
	f.p.claims = map[string]any{"roles": []any{"a"}, "groups": "g"}
	cfg := f.rp.Config()
	for _, tc := range []struct{ claim, want string }{{"roles", "a"}, {"groups", ""}, {"", ""}} {
		cfg.GroupsClaim = tc.claim
		if w := f.login("good"); w.Code != http.StatusFound {
			t.Fatalf("status %d", w.Code)
		}
		c := f.rec.provisioned[len(f.rec.provisioned)-1]
		if strings.Join(c.Groups, ",") != tc.want || (tc.claim == "" && c.Groups != nil) {
			t.Errorf("claim %q: groups = %#v", tc.claim, c.Groups)
		}
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

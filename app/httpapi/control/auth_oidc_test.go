package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/kv"
)

// fakeIdP is an OIDC provider: discovery, JWKS and a token endpoint that checks the code and returns an EdDSA-signed id_token.
type fakeIdP struct {
	srv        *httptest.Server
	issuedCode string
	sub        string
	email      string
	key        ed25519.PrivateKey

	// claims override or add id_token claims; a nil value removes one.
	claims map[string]any
	// signWith, when set, signs the id_token with a key the JWKS does not publish.
	signWith ed25519.PrivateKey

	// inlineProfile switches the token response from a standard id_token to
	// the hosted-user-management shape: a {"user": {...}} object and no JWT.
	inlineProfile bool

	// nonce is the authorize request's nonce, which the id_token echoes.
	nonce string

	sawVerifier  string
	sawSecret    string
	discoveryHit atomic.Int32
	jwksHits     atomic.Int32
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{issuedCode: "code-123", sub: "user_abc", email: "alice@example.com", key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		f.discoveryHit.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		f.jwksHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey)),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != f.issuedCode {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		f.sawVerifier = r.Form.Get("code_verifier")
		f.sawSecret = r.Form.Get("client_secret")
		if f.sawSecret == "" {
			if _, s, ok := r.BasicAuth(); ok {
				f.sawSecret = s
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if f.inlineProfile {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-xyz",
				"token_type":   "bearer",
				"user":         map[string]any{"id": f.sub, "email": f.email},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-xyz",
			"token_type":   "bearer",
			"id_token":     f.idToken(),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) idToken() string {
	now := time.Now()
	claims := map[string]any{
		"iss":    f.srv.URL,
		"aud":    "client-1",
		"sub":    f.sub,
		"email":  f.email,
		"sid":    "sid-42",
		"iat":    now.Unix(),
		"exp":    now.Add(5 * time.Minute).Unix(),
		"groups": []string{"eng", "ops"},
		"nonce":  f.nonce,
	}
	for k, v := range f.claims {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "k1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	key := f.key
	if f.signWith != nil {
		key = f.signWith
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}

// fakeUsers is an in-memory oidcUserStore.
type fakeUsers struct {
	bySubject map[string]*user.User
	byName    map[string]*user.User
	// external is the SCIM externalId of a provisioned account, by user id.
	external map[string]string
	// beforeLink, when set, runs once just before AddFirstIdentity writes.
	beforeLink func()
	links      int
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{bySubject: map[string]*user.User{}, byName: map[string]*user.User{}, external: map[string]string{}}
}

// provisioned adds an account as a SCIM client creates it: no identity, the client's externalId.
func (f *fakeUsers) provisioned(u *user.User, externalID string) {
	f.byName[u.Username] = u
	f.external[u.ID] = externalID
}

func (f *fakeUsers) UnlinkedByExternalID(_ context.Context, externalID string) ([]*user.User, error) {
	var out []*user.User
	for _, u := range f.byName {
		if id, ok := f.external[u.ID]; ok && id == externalID && u.OIDCSubject == "" {
			c := *u
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeUsers) AddFirstIdentity(_ context.Context, userID string, id directory.Identity) error {
	if hook := f.beforeLink; hook != nil {
		f.beforeLink = nil
		hook()
	}
	for _, u := range f.byName {
		if u.ID != userID {
			continue
		}
		if u.OIDCSubject != "" {
			return directory.ErrConflict
		}
		u.OIDCSubject = id.Issuer + directory.IdentitySeparator + id.Subject
		f.bySubject[u.OIDCSubject] = u
		f.links++
		return nil
	}
	return directory.ErrNotFound
}

func (f *fakeUsers) ByUsername(_ context.Context, u string) (*user.User, error) {
	return f.byName[u], nil
}

func (f *fakeUsers) ByOIDCSubject(_ context.Context, s string) (*user.User, error) {
	return f.bySubject[s], nil
}

func (f *fakeUsers) Create(ctx context.Context, u *user.User) error {
	for _, other := range f.byName {
		if u.Email != "" && other.Email == u.Email {
			return directory.ErrEmailTaken
		}
	}
	return f.Upsert(ctx, u)
}

func (f *fakeUsers) Upsert(_ context.Context, u *user.User) error {
	f.bySubject[u.OIDCSubject] = u
	f.byName[u.Username] = u
	return nil
}

// fakeSessions records the LoginOIDC call.
type fakeSessions struct {
	userID, username string
	subject, sid     string
	groups           []string
	roles            []string
	calls            int
}

func (f *fakeSessions) LoginOIDC(_ context.Context, userID, username, oidcSubject, idpSessionID string, groups []string, roles ...string) error {
	f.userID, f.username, f.roles = userID, username, roles
	f.subject, f.sid, f.groups = oidcSubject, idpSessionID, groups
	f.calls++
	return nil
}

// oidcHarness serves the OIDC routes as the control listener mounts them: start and callback under /api behind the audit middleware, and the root /auth/callback.
type oidcHarness struct {
	t     *testing.T
	idp   *fakeIdP
	cfg   *settings.AuthOIDC
	scim  *settings.AuthSCIM
	users *fakeUsers
	sess  *fakeSessions
	sink  *auditSink
	em    *audit.Emitter
	h     http.Handler
}

func newTestOIDC(t *testing.T, idp *fakeIdP, registration string) *oidcHarness {
	t.Helper()
	return newOIDCHarness(t, idp, &settings.AuthOIDC{
		Enabled:      true,
		Issuer:       idp.srv.URL,
		ClientID:     "client-1",
		RedirectURL:  "http://relay.test/api/auth/oidc/callback",
		Registration: registration,
	})
}

func newOIDCHarness(t *testing.T, idp *fakeIdP, cfg *settings.AuthOIDC) *oidcHarness {
	t.Helper()
	users := newFakeUsers()
	h := newOIDCHarnessOver(t, idp, cfg, users, nil)
	h.users = users
	return h
}

// newOIDCHarnessOver serves the routes over users, auditing to sink (h.sink when nil); SCIM is off until the test turns h.scim on.
func newOIDCHarnessOver(t *testing.T, idp *fakeIdP, cfg *settings.AuthOIDC, users oidcUserStore, sink audit.Sink) *oidcHarness {
	t.Helper()
	h := &oidcHarness{t: t, idp: idp, cfg: cfg, scim: &settings.AuthSCIM{}, sess: &fakeSessions{}, sink: &auditSink{}}
	if sink == nil {
		sink = h.sink
	}
	h.em = audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	od := newOIDC(func() *settings.AuthOIDC { return h.cfg }, func() *settings.AuthSCIM { return h.scim }, users, h.sess, false)
	r := chi.NewRouter()
	r.Route("/api", func(api chi.Router) {
		api.Use(audit.Middleware(h.em, nil))
		mountOIDCRoutes(api, od)
	})
	mountOIDCCallbackRoot(r, Deps{Sessions: session.New(kv.NewMem(), false, "sess:"), Audit: h.em}, od)
	h.h = r
	return h
}

func (h *oidcHarness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// start runs the start handler, hands the authorize request's nonce to the IdP, and returns the IdP redirect URL and the flow cookie.
func (h *oidcHarness) start() (*url.URL, *http.Cookie) {
	h.t.Helper()
	rec := h.do(httptest.NewRequest("GET", "/api/auth/oidc/start", nil))
	if rec.Code != http.StatusFound {
		h.t.Fatalf("start: status %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.idp.nonce = loc.Query().Get("nonce")
	flow := cookieNamed(rec, oidcFlowCookie)
	if flow == nil {
		h.t.Fatal("start set no flow cookie")
	}
	return loc, flow
}

// callback hits path with query, carrying flow when non-nil.
func (h *oidcHarness) callback(path, query string, flow *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path+"?"+query, nil)
	if flow != nil {
		req.AddCookie(flow)
	}
	return h.do(req)
}

// login runs start and then the callback at path with the IdP's code and the issued state.
func (h *oidcHarness) login(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	loc, flow := h.start()
	return h.callback(path, "code="+h.idp.issuedCode+"&state="+url.QueryEscape(loc.Query().Get("state")), flow)
}

// events closes the emitter and returns what it recorded; call it once, last.
func (h *oidcHarness) events() []audit.Event {
	h.em.Close()
	return h.sink.all()
}

func (h *oidcHarness) subject() string { return h.idp.srv.URL + "|" + h.idp.sub }

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func wantDenied(t *testing.T, evs []audit.Event, name string) {
	t.Helper()
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one denied auth.login", evs)
	}
	ev := evs[0]
	if ev.Action != "auth.login" || ev.Outcome.Status != audit.StatusDenied || ev.Actor.Name != name || ev.Resource.Name != name {
		t.Fatalf("event = %+v, want auth.login denied naming %q", ev, name)
	}
}

func wantResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || strings.TrimSpace(rec.Body.String()) != body {
		t.Fatalf("response = %d %q, want %d %q", rec.Code, rec.Body.String(), status, body)
	}
}

// The authorize redirect carries state, a PKCE S256 challenge and a nonce; the flow cookie holds all three, is HttpOnly, SameSite=Lax, Path=/ and lives ten minutes.
func TestOIDCStart_RedirectAndFlowCookie(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	h.cfg.AuthParams = map[string]string{"provider": "authkit"}

	rec := h.do(httptest.NewRequest("GET", "/api/auth/oidc/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: status %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != idp.srv.URL+"/authorize" {
		t.Fatalf("authorize endpoint = %q", got)
	}
	q := loc.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             "client-1",
		"redirect_uri":          "http://relay.test/api/auth/oidc/callback",
		"scope":                 "openid profile email",
		"code_challenge_method": "S256",
		"provider":              "authkit",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("state")) != 32 || q.Get("code_challenge") == "" {
		t.Errorf("state %q / code_challenge %q", q.Get("state"), q.Get("code_challenge"))
	}
	if len(q.Get("nonce")) != 32 {
		t.Errorf("authorize nonce = %q", q.Get("nonce"))
	}

	c := cookieNamed(rec, "relay_oidc_flow")
	if c == nil {
		t.Fatal("no relay_oidc_flow cookie")
	}
	if c.Path != "/" || c.MaxAge != 600 || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure {
		t.Fatalf("flow cookie attributes = %+v", c)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatal(err)
	}
	var fs struct {
		S string `json:"s"`
		V string `json:"v"`
		N string `json:"n"`
	}
	if err := json.Unmarshal(raw, &fs); err != nil || fs.S != q.Get("state") || fs.V == "" || fs.N != q.Get("nonce") {
		t.Fatalf("flow cookie payload = %s", raw)
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("start recorded %d audit events", len(evs))
	}
}

func TestOIDCStart_SecureCookie(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	r := chi.NewRouter()
	mountOIDCRoutes(r, newOIDC(func() *settings.AuthOIDC { return h.cfg }, func() *settings.AuthSCIM { return h.scim }, h.users, h.sess, true))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/oidc/start", nil))
	if c := cookieNamed(rec, oidcFlowCookie); c == nil || !c.Secure {
		t.Fatalf("flow cookie = %+v, want Secure", c)
	}
}

func TestOIDCStart_DiscoveryFailure(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	h.cfg.Issuer = idp.srv.URL + "/missing"
	wantResponse(t, h.do(httptest.NewRequest("GET", "/api/auth/oidc/start", nil)), http.StatusBadGateway, "identity provider unavailable")
}

// Happy path on both mounts: the session gets the issuer-qualified subject, the IdP sid and the groups; the flow cookie is cleared; the browser lands on "/".
func TestOIDCFlow_ProvisionsAndLogsIn(t *testing.T) {
	for _, path := range []string{"/api/auth/oidc/callback", "/auth/callback"} {
		t.Run(path, func(t *testing.T) {
			idp := newFakeIdP(t)
			h := newTestOIDC(t, idp, "open")

			rec := h.login(path)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
				t.Fatalf("callback: status %d location %q body %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
			}
			if c := cookieNamed(rec, oidcFlowCookie); c == nil || c.MaxAge != -1 || c.Value != "" {
				t.Errorf("flow cookie not cleared: %+v", c)
			}
			if idp.sawVerifier == "" {
				t.Error("token exchange sent no PKCE verifier")
			}
			u := h.users.bySubject[h.subject()]
			if u == nil {
				t.Fatal("user not provisioned")
			}
			if u.Username != "alice" || u.Email != idp.email || u.ID == "" {
				t.Errorf("provisioned user wrong: %+v", u)
			}
			s := h.sess
			if s.calls != 1 || s.userID != u.ID || s.username != "alice" {
				t.Errorf("session login wrong: %+v", s)
			}
			if s.subject != h.subject() || s.sid != "sid-42" {
				t.Errorf("session missing IdP sub/sid: subject=%q sid=%q", s.subject, s.sid)
			}
			if strings.Join(s.groups, ",") != "eng,ops" {
				t.Errorf("session groups = %q", s.groups)
			}
			if idp.jwksHits.Load() != 1 {
				t.Errorf("JWKS fetched %d times", idp.jwksHits.Load())
			}

			// Second login with the same subject reuses the row and the cached signing keys.
			if rec := h.login(path); rec.Code != http.StatusFound {
				t.Fatalf("second callback: status %d", rec.Code)
			}
			if len(h.users.bySubject) != 1 {
				t.Errorf("second login created a duplicate user")
			}
			if idp.jwksHits.Load() != 1 {
				t.Errorf("JWKS fetched %d times over two logins", idp.jwksHits.Load())
			}
		})
	}
}

// An existing user logs in with its stored roles, and provisioning derives the username from preferred_username first, then the email local part, then "user", with a collision suffix.
func TestOIDCFlow_UsernameAndRoles(t *testing.T) {
	t.Run("existing user keeps roles", func(t *testing.T) {
		idp := newFakeIdP(t)
		h := newTestOIDC(t, idp, "closed")
		_ = h.users.Upsert(context.Background(), &user.User{ID: "u1", Username: "al", OIDCSubject: h.subject(), Roles: []string{"admin"}})
		if rec := h.login("/api/auth/oidc/callback"); rec.Code != http.StatusFound {
			t.Fatalf("status %d", rec.Code)
		}
		if h.sess.userID != "u1" || h.sess.username != "al" || strings.Join(h.sess.roles, ",") != "admin" {
			t.Fatalf("session = %+v", h.sess)
		}
	})
	cases := []struct {
		name   string
		claims map[string]any
		taken  string
		want   string
	}{
		{"preferred_username", map[string]any{"preferred_username": "Bob Smith"}, "", "bob-smith"},
		{"email local part", nil, "", "alice"},
		{"collision suffix", nil, "alice", "alice-2"},
		{"fallback", map[string]any{"email": nil}, "", "user"},
		// Zitadel without idTokenUserinfoAssertion signs an id_token carrying sub alone.
		{"sub-only id_token", map[string]any{"email": nil, "sid": nil, "groups": nil}, "", "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.claims = tc.claims
			h := newTestOIDC(t, idp, "open")
			if tc.taken != "" {
				_ = h.users.Upsert(context.Background(), &user.User{ID: "x", Username: tc.taken, OIDCSubject: "other|x"})
			}
			if rec := h.login("/api/auth/oidc/callback"); rec.Code != http.StatusFound {
				t.Fatalf("status %d", rec.Code)
			}
			if u := h.users.bySubject[h.subject()]; u == nil || u.Username != tc.want {
				t.Fatalf("username = %+v, want %q", u, tc.want)
			}
		})
	}
}

// The id_token is verified before any claim is read: a bad signature, audience, issuer, expiry or nonce is a 502 with a denied audit row and no session.
func TestOIDCFlow_IDTokenRejected(t *testing.T) {
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func(*fakeIdP){
		"wrong signature": func(f *fakeIdP) { f.signWith = otherKey },
		"wrong audience":  func(f *fakeIdP) { f.claims = map[string]any{"aud": "someone-else"} },
		"wrong issuer":    func(f *fakeIdP) { f.claims = map[string]any{"iss": "https://evil.example"} },
		"expired":         func(f *fakeIdP) { f.claims = map[string]any{"exp": time.Now().Add(-time.Hour).Unix()} },
		"wrong nonce":     func(f *fakeIdP) { f.claims = map[string]any{"nonce": "attacker"} },
		"missing nonce":   func(f *fakeIdP) { f.claims = map[string]any{"nonce": nil} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIdP(t)
			mutate(idp)
			h := newTestOIDC(t, idp, "open")
			wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusBadGateway, "identity provider returned an invalid id_token")
			if h.sess.calls != 0 || len(h.users.bySubject) != 0 {
				t.Fatalf("session = %+v, users = %d", h.sess, len(h.users.bySubject))
			}
			wantDenied(t, h.events(), "")
		})
	}
}

// The id_token payload must be a JWT with a sub; anything else is a 502 and a denied audit row.
func TestOIDCFlow_UnusableIDToken(t *testing.T) {
	idp := newFakeIdP(t)
	idp.claims = map[string]any{"sub": nil}
	h := newTestOIDC(t, idp, "open")
	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusBadGateway, "identity provider returned no usable identity")
	if h.sess.calls != 0 {
		t.Error("session minted without a subject")
	}
	wantDenied(t, h.events(), "")
}

func TestOIDCFlow_GroupsClaim(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name   string
		claim  *string
		claims map[string]any
		want   string
	}{
		{"default claim", nil, nil, "eng,ops"},
		{"configured claim", str("roles"), map[string]any{"roles": []any{"r1", "r2"}}, "r1,r2"},
		{"configured claim absent", str("roles"), nil, ""},
		{"opted out", str(""), nil, ""},
		{"non-strings dropped", nil, map[string]any{"groups": []any{"a", 7, "", "b"}}, "a,b"},
		{"not an array", nil, map[string]any{"groups": "eng"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.claims = tc.claims
			h := newTestOIDC(t, idp, "open")
			h.cfg.GroupsClaim = tc.claim
			if rec := h.login("/api/auth/oidc/callback"); rec.Code != http.StatusFound {
				t.Fatalf("status %d", rec.Code)
			}
			if got := strings.Join(h.sess.groups, ","); got != tc.want {
				t.Fatalf("groups = %q, want %q", got, tc.want)
			}
		})
	}
}

// A configured PostLoginURL wins over the default "/" redirect — the
// cross-origin-UI topology sends the browser back to the UI origin instead
// of stranding it on the control origin.
func TestOIDCFlow_PostLoginURLRedirect(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	h.cfg.PostLoginURL = "https://ui.test/welcome"

	rec := h.login("/auth/callback")
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "https://ui.test/welcome" {
		t.Fatalf("post-login redirect: want configured URL, got %q", got)
	}
}

// Providers that inline the profile on the token response instead of an
// id_token (the WorkOS-style hosted user-management shape) log in too.
func TestOIDCFlow_InlineProfileTokenResponse(t *testing.T) {
	idp := newFakeIdP(t)
	idp.inlineProfile = true
	h := newTestOIDC(t, idp, "open")

	if rec := h.login("/api/auth/oidc/callback"); rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
	}
	u := h.users.bySubject[h.subject()]
	if u == nil || u.Email != idp.email {
		t.Fatalf("user not provisioned from inline profile: %+v", u)
	}
	if h.sess.calls != 1 || h.sess.sid != "" || len(h.sess.groups) != 0 {
		t.Errorf("session = %+v", h.sess)
	}
	if idp.jwksHits.Load() != 0 {
		t.Errorf("JWKS fetched %d times without an id_token", idp.jwksHits.Load())
	}
}

func TestOIDCFlow_ClosedRegistrationRejectsUnknown(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "closed")

	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusForbidden, "no account for this identity (registration is closed)")
	if h.sess.calls != 0 || len(h.users.bySubject) != 0 {
		t.Error("session minted or user created despite closed registration")
	}
	wantDenied(t, h.events(), h.subject())
}

// A first sign-in whose email belongs to an account with no identity linked, as SCIM or YAML creates, is refused like closed registration rather than linked or answered with a 500.
func TestOIDCFlow_EmailTakenRejected(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	_ = h.users.Upsert(context.Background(), &user.User{ID: "u1", Username: "alice-scim", Email: idp.email})

	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusForbidden, "an account with this email already exists; an administrator must link it to this sign-in")
	if h.sess.calls != 0 || h.users.bySubject[h.subject()] != nil || h.users.byName["alice-scim"].OIDCSubject != "" {
		t.Error("session minted, account created or identity linked despite the taken email")
	}
	wantDenied(t, h.events(), h.subject())
}

func TestOIDCFlow_StateMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")

	_, flow := h.start()
	rec := h.callback("/api/auth/oidc/callback", "code="+idp.issuedCode+"&state=forged", flow)
	wantResponse(t, rec, http.StatusBadRequest, "state mismatch")
	if c := cookieNamed(rec, oidcFlowCookie); c == nil || c.MaxAge != -1 {
		t.Errorf("flow cookie not cleared on mismatch: %+v", c)
	}
	if rec := h.callback("/api/auth/oidc/callback", "code="+idp.issuedCode, flow); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing state: status %d", rec.Code)
	}
	if h.sess.calls != 0 {
		t.Error("session minted on forged state")
	}
}

// A missing, expired (the browser drops it after MaxAge) or garbled flow cookie means the dance has to start over.
func TestOIDCFlow_FlowCookieRejected(t *testing.T) {
	cases := map[string]*http.Cookie{
		"missing":     nil,
		"empty":       {Name: oidcFlowCookie, Value: ""},
		"not base64":  {Name: oidcFlowCookie, Value: "%%%"},
		"not json":    {Name: oidcFlowCookie, Value: base64.RawURLEncoding.EncodeToString([]byte("nope"))},
		"no verifier": {Name: oidcFlowCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(`{"s":"y"}`))},
		"no state":    {Name: oidcFlowCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(`{"v":"v"}`))},
		// Set before nonces existed: that one login restarts.
		"no nonce":   {Name: oidcFlowCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(`{"s":"y","v":"v"}`))},
		"other name": {Name: "relay_session", Value: base64.RawURLEncoding.EncodeToString([]byte(`{"s":"y","v":"v"}`))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newTestOIDC(t, newFakeIdP(t), "open")
			wantResponse(t, h.callback("/api/auth/oidc/callback", "code=x&state=y", c), http.StatusBadRequest, "login flow expired — start again")
			wantDenied(t, h.events(), "")
		})
	}
}

func TestOIDCFlow_MissingCode(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	loc, flow := h.start()
	wantResponse(t, h.callback("/api/auth/oidc/callback", "state="+loc.Query().Get("state"), flow), http.StatusBadRequest, "missing code")
	wantDenied(t, h.events(), "")
}

func TestOIDCFlow_ProviderError(t *testing.T) {
	h := newTestOIDC(t, newFakeIdP(t), "open")
	_, flow := h.start()
	rec := h.callback("/api/auth/oidc/callback", "error=access_denied&error_description=nope", flow)
	wantResponse(t, rec, http.StatusBadGateway, "login failed at identity provider: access_denied")
	if c := cookieNamed(rec, oidcFlowCookie); c != nil {
		t.Errorf("provider error touched the flow cookie: %+v", c)
	}
	wantDenied(t, h.events(), "")
}

func TestOIDCFlow_CodeExchangeFailure(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	loc, flow := h.start()
	wantResponse(t, h.callback("/api/auth/oidc/callback", "code=wrong&state="+loc.Query().Get("state"), flow), http.StatusBadGateway, "code exchange failed")
	wantDenied(t, h.events(), "")
}

// Discovery is cached per issuer, client and redirect URL; a callback whose discovery fails is a 502 with no audit row.
func TestOIDCFlow_Discovery(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	for range 2 {
		if rec := h.login("/api/auth/oidc/callback"); rec.Code != http.StatusFound {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if n := idp.discoveryHit.Load(); n != 1 {
		t.Fatalf("discovery fetched %d times, want 1", n)
	}

	loc, flow := h.start()
	h.cfg.Issuer = idp.srv.URL + "/missing"
	wantResponse(t, h.callback("/api/auth/oidc/callback", "code="+idp.issuedCode+"&state="+loc.Query().Get("state"), flow), http.StatusBadGateway, "identity provider unavailable")
	if evs := h.events(); len(evs) != 2 {
		t.Fatalf("events = %d, want the two logins only", len(evs))
	}
}

func TestOIDCFlow_DisabledIs404(t *testing.T) {
	h := newOIDCHarness(t, newFakeIdP(t), &settings.AuthOIDC{})
	for _, path := range []string{"/api/auth/oidc/start", "/api/auth/oidc/callback", "/auth/callback"} {
		wantResponse(t, h.do(httptest.NewRequest("GET", path, nil)), http.StatusNotFound, "oidc login is not enabled")
	}
}

func TestOIDCFlow_DisabledUserRejected(t *testing.T) {
	idp := newFakeIdP(t)
	h := newTestOIDC(t, idp, "open")
	_ = h.users.Upsert(context.Background(), &user.User{
		ID: "u1", Username: "alice", OIDCSubject: h.subject(), Disabled: true,
	})

	wantResponse(t, h.login("/api/auth/oidc/callback"), http.StatusUnauthorized, "account is disabled; contact your administrator")
	if h.sess.calls != 0 {
		t.Error("session minted for disabled user")
	}
	wantDenied(t, h.events(), "alice")
}

// The callback records one audit row per attempt: allowed naming the
// logged-in user on success, denied on every rejection path. The root
// callback is mounted outside the /api group, so it has to bring the audit
// middleware itself or SSO logins through it leave no row.
func TestOIDCFlow_AuditsLogin(t *testing.T) {
	for _, path := range []string{"/api/auth/oidc/callback", "/auth/callback"} {
		t.Run(path, func(t *testing.T) {
			h := newTestOIDC(t, newFakeIdP(t), "open")
			if rec := h.login(path); rec.Code != http.StatusFound {
				t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
			}
			evs := h.events()
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			ev := evs[0]
			u := h.users.byName["alice"]
			if ev.Action != "auth.login" || ev.Outcome.Status != audit.StatusAllowed {
				t.Fatalf("event = action %q status %q, want auth.login allowed", ev.Action, ev.Outcome.Status)
			}
			if ev.Actor.Kind != audit.ActorUser || ev.Actor.Name != "alice" || ev.Actor.ID != u.ID || ev.Resource.ID != u.ID || ev.Resource.Kind != "user" {
				t.Fatalf("event = %+v, want the provisioned user alice", ev)
			}
		})
	}
}

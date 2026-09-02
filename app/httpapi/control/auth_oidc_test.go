package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/sdk/oauth"
)

// fakeIdP is a minimal OIDC provider: OIDC discovery + a token endpoint
// that validates the code/verifier shape and returns an (unsigned) id_token.
type fakeIdP struct {
	srv        *httptest.Server
	issuedCode string
	sub        string
	email      string

	// inlineProfile switches the token response from a standard id_token to
	// the hosted-user-management shape: a {"user": {...}} object and no JWT.
	inlineProfile bool

	sawVerifier string
	sawSecret   string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{issuedCode: "code-123", sub: "user_abc", email: "alice@example.com"}
	mux := http.NewServeMux()
	// Deliberately only the OIDC discovery path (the WorkOS shape) so the
	// RFC 8414 → openid-configuration fallback is exercised end to end.
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
		})
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
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
			`{"sub":%q,"email":%q,"sid":"sid-42"}`, f.sub, f.email)))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-xyz",
			"token_type":   "bearer",
			"id_token":     header + "." + payload + ".sig",
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// fakeUsers is an in-memory oidcUserStore.
type fakeUsers struct {
	bySubject map[string]*user.User
	byName    map[string]*user.User
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{bySubject: map[string]*user.User{}, byName: map[string]*user.User{}}
}

func (f *fakeUsers) ByUsername(_ context.Context, u string) (*user.User, error) {
	return f.byName[u], nil
}

func (f *fakeUsers) ByOIDCSubject(_ context.Context, s string) (*user.User, error) {
	return f.bySubject[s], nil
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

func newTestOIDC(idp *fakeIdP, users *fakeUsers, sess *fakeSessions, registration string) *oidcDeps {
	cfg := &settings.AuthOIDC{
		Enabled:      true,
		Issuer:       idp.srv.URL,
		ClientID:     "client-1",
		RedirectURL:  "http://relay.test/api/auth/oidc/callback",
		Registration: registration,
	}
	return &oidcDeps{
		cfg:          func() *settings.AuthOIDC { return cfg },
		users:        users,
		sessions:     sess,
		cookieSecure: false,
		discover:     cachedDiscover(),
	}
}

// driveStart runs the start handler and returns the IdP redirect URL and the
// flow cookie.
func driveStart(t *testing.T, od *oidcDeps) (*url.URL, *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	od.start(rec, httptest.NewRequest("GET", "/api/auth/oidc/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	var flow *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcFlowCookie {
			flow = c
		}
	}
	if flow == nil {
		t.Fatal("start set no flow cookie")
	}
	return loc, flow
}

// A configured PostLoginURL wins over the default "/" redirect — the
// cross-origin-UI topology sends the browser back to the UI origin instead
// of stranding it on the control origin.
func TestOIDCFlow_PostLoginURLRedirect(t *testing.T) {
	idp := newFakeIdP(t)
	od := newTestOIDC(idp, newFakeUsers(), &fakeSessions{}, "open")
	od.cfg().PostLoginURL = "https://ui.test/welcome"

	loc, flow := driveStart(t, od)
	cb := httptest.NewRequest("GET",
		"/auth/callback?code="+idp.issuedCode+"&state="+loc.Query().Get("state"), nil)
	cb.AddCookie(flow)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "https://ui.test/welcome" {
		t.Fatalf("post-login redirect: want configured URL, got %q", got)
	}
}

func TestOIDCFlow_ProvisionsAndLogsIn(t *testing.T) {
	idp := newFakeIdP(t)
	users := newFakeUsers()
	sess := &fakeSessions{}
	od := newTestOIDC(idp, users, sess, "open")

	loc, flow := driveStart(t, od)
	q := loc.Query()
	if q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("client_id") != "client-1" {
		t.Fatalf("authorize URL missing params: %s", loc)
	}

	// Simulate the IdP redirecting back with the code.
	cb := httptest.NewRequest("GET",
		"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+q.Get("state"), nil)
	cb.AddCookie(flow)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
	}
	if idp.sawVerifier == "" {
		t.Error("token exchange sent no PKCE verifier")
	}
	u := users.bySubject[idp.srv.URL+"|"+idp.sub]
	if u == nil {
		t.Fatal("user not provisioned")
	}
	if u.Username != "alice" || u.Email != idp.email {
		t.Errorf("provisioned user wrong: %+v", u)
	}
	if sess.calls != 1 || sess.userID != u.ID {
		t.Errorf("session login wrong: %+v", sess)
	}
	if sess.subject != idp.srv.URL+"|"+idp.sub || sess.sid != "sid-42" {
		t.Errorf("session missing IdP sub/sid: subject=%q sid=%q", sess.subject, sess.sid)
	}

	// Second login with the same subject reuses the row.
	loc2, flow2 := driveStart(t, od)
	cb2 := httptest.NewRequest("GET",
		"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+loc2.Query().Get("state"), nil)
	cb2.AddCookie(flow2)
	rec2 := httptest.NewRecorder()
	od.callback(rec2, cb2)
	if rec2.Code != http.StatusFound {
		t.Fatalf("second callback: status %d", rec2.Code)
	}
	if len(users.bySubject) != 1 {
		t.Errorf("second login created a duplicate user")
	}
}

// Providers that inline the profile on the token response instead of an
// id_token (the WorkOS-style hosted user-management shape) log in too.
func TestOIDCFlow_InlineProfileTokenResponse(t *testing.T) {
	idp := newFakeIdP(t)
	idp.inlineProfile = true
	users := newFakeUsers()
	sess := &fakeSessions{}
	od := newTestOIDC(idp, users, sess, "open")

	loc, flow := driveStart(t, od)
	cb := httptest.NewRequest("GET",
		"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+loc.Query().Get("state"), nil)
	cb.AddCookie(flow)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
	}
	u := users.bySubject[idp.srv.URL+"|"+idp.sub]
	if u == nil || u.Email != idp.email {
		t.Fatalf("user not provisioned from inline profile: %+v", u)
	}
	if sess.calls != 1 {
		t.Errorf("session not minted: %+v", sess)
	}
}

func TestOIDCFlow_ClosedRegistrationRejectsUnknown(t *testing.T) {
	idp := newFakeIdP(t)
	sess := &fakeSessions{}
	od := newTestOIDC(idp, newFakeUsers(), sess, "closed")

	loc, flow := driveStart(t, od)
	cb := httptest.NewRequest("GET",
		"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+loc.Query().Get("state"), nil)
	cb.AddCookie(flow)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("closed registration: status %d, want 403", rec.Code)
	}
	if sess.calls != 0 {
		t.Error("session minted despite closed registration")
	}
}

func TestOIDCFlow_StateMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	od := newTestOIDC(idp, newFakeUsers(), &fakeSessions{}, "open")

	_, flow := driveStart(t, od)
	cb := httptest.NewRequest("GET",
		"/api/auth/oidc/callback?code="+idp.issuedCode+"&state=forged", nil)
	cb.AddCookie(flow)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state: status %d, want 400", rec.Code)
	}
}

func TestOIDCFlow_MissingCookieRejected(t *testing.T) {
	idp := newFakeIdP(t)
	od := newTestOIDC(idp, newFakeUsers(), &fakeSessions{}, "open")

	cb := httptest.NewRequest("GET", "/api/auth/oidc/callback?code=x&state=y", nil)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no cookie: status %d, want 400", rec.Code)
	}
}

func TestOIDCFlow_DisabledIs404(t *testing.T) {
	od := &oidcDeps{cfg: func() *settings.AuthOIDC { return &settings.AuthOIDC{} }}
	rec := httptest.NewRecorder()
	od.start(rec, httptest.NewRequest("GET", "/api/auth/oidc/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled start: status %d, want 404", rec.Code)
	}
}

func TestOIDCFlow_DisabledUserRejected(t *testing.T) {
	idp := newFakeIdP(t)
	users := newFakeUsers()
	_ = users.Upsert(context.Background(), &user.User{
		ID: "u1", Username: "alice", OIDCSubject: idp.srv.URL + "|" + idp.sub, Disabled: true,
	})
	sess := &fakeSessions{}
	od := newTestOIDC(idp, users, sess, "open")

	loc, flow := driveStart(t, od)
	cb := httptest.NewRequest("GET",
		"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+loc.Query().Get("state"), nil)
	cb.AddCookie(flow)
	rec := httptest.NewRecorder()
	od.callback(rec, cb)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled user: status %d, want 403", rec.Code)
	}
	if sess.calls != 0 {
		t.Error("session minted for disabled user")
	}
}

// The callback records one audit row per attempt: allowed naming the
// logged-in user on success, denied on every rejection path.
func TestOIDCFlow_AuditsLogin(t *testing.T) {
	t.Run("success names the logged-in user", func(t *testing.T) {
		idp := newFakeIdP(t)
		od := newTestOIDC(idp, newFakeUsers(), &fakeSessions{}, "open")

		sink := &auditSink{}
		em := audit.NewEmitter(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
		h := audit.Middleware(em, nil)(http.HandlerFunc(od.callback))

		loc, flow := driveStart(t, od)
		cb := httptest.NewRequest("GET",
			"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+loc.Query().Get("state"), nil)
		cb.AddCookie(flow)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, cb)
		if rec.Code != http.StatusFound {
			t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body.String())
		}
		em.Close()

		evs := sink.all()
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		ev := evs[0]
		if ev.Action != "auth.login" || ev.Outcome.Status != audit.StatusAllowed {
			t.Fatalf("event = action %q status %q, want auth.login allowed", ev.Action, ev.Outcome.Status)
		}
		if ev.Actor.Name != "alice" {
			t.Fatalf("actor = %+v, want the provisioned user alice", ev.Actor)
		}
	})

	t.Run("rejection is denied", func(t *testing.T) {
		idp := newFakeIdP(t)
		od := newTestOIDC(idp, newFakeUsers(), &fakeSessions{}, "closed")

		sink := &auditSink{}
		em := audit.NewEmitter(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
		h := audit.Middleware(em, nil)(http.HandlerFunc(od.callback))

		loc, flow := driveStart(t, od)
		cb := httptest.NewRequest("GET",
			"/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+loc.Query().Get("state"), nil)
		cb.AddCookie(flow)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, cb)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("closed registration: status %d, want 403", rec.Code)
		}
		em.Close()

		evs := sink.all()
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		if ev := evs[0]; ev.Action != "auth.login" || ev.Outcome.Status != audit.StatusDenied {
			t.Fatalf("event = action %q status %q, want auth.login denied", ev.Action, ev.Outcome.Status)
		}
	})
}

// Guard: the id_token parser rejects garbage tokens.
func TestIDTokenClaims(t *testing.T) {
	mk := func(payload string) *oauth.Token {
		tok := &oauth.Token{}
		raw := "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".s"
		return tok.WithExtra(map[string]any{"id_token": raw})
	}
	if _, err := idTokenClaims(mk(`{"sub":"x"}`)); err != nil {
		t.Errorf("valid claims rejected: %v", err)
	}
	if _, err := idTokenClaims(mk(`{}`)); err == nil || !strings.Contains(err.Error(), "sub") {
		t.Errorf("missing sub accepted: %v", err)
	}
	if _, err := idTokenClaims(&oauth.Token{}); err == nil {
		t.Error("absent id_token accepted")
	}
}

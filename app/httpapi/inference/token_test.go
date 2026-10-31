package inference

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/httpheader"
)

// TestLooksLikeToken pins the bearer shape sniff (a key carries no dots, a
// compact JWT always carries two after an "ey" header).
func TestLooksLikeToken(t *testing.T) {
	for _, tc := range []struct {
		bearer string
		want   bool
	}{
		{bearer: "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJyZWxheSJ9.sig", want: true},
		{bearer: "sk-wr-live-abcdef", want: false},
		{bearer: "sk-wr.two.dots", want: false},
		{bearer: "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJyZWxheSJ9", want: false},
		{bearer: "", want: false},
	} {
		if got := looksLikeToken(tc.bearer); got != tc.want {
			t.Errorf("looksLikeToken(%q) = %v, want %v", tc.bearer, got, tc.want)
		}
	}
}

// TestTokenPrincipal_Rejections walks the token column of the auth table:
// each way a token can fail, and the status the caller sees.
func TestTokenPrincipal_Rejections(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated")}
	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	for _, tc := range []struct {
		name       string
		token      func() string
		wantStatus int
		wantMsg    string
		wantCode   string // checked when set
	}{
		{
			name:       "another key's signature",
			token:      func() string { return signedWith(t, f, otherPriv, "") },
			wantStatus: http.StatusUnauthorized, wantMsg: "invalid token",
		},
		{
			name:       "not issued by this relay",
			token:      func() string { return f.mint(t, func(c *crypto.TokenClaims) { c.Iss = "someone-else" }) },
			wantStatus: http.StatusUnauthorized, wantMsg: "invalid token",
		},
		{
			name: "expired",
			token: func() string {
				return f.mint(t, func(c *crypto.TokenClaims) { c.Exp = time.Now().Add(-time.Minute).Unix() })
			},
			wantStatus: http.StatusUnauthorized, wantMsg: "token expired",
		},
		{
			// Signing out everywhere (a token-version bump) answers the same
			// code as a per-jti revocation, so a client has one thing to key on.
			name:       "token version behind the user's",
			token:      func() string { return f.mint(t, func(c *crypto.TokenClaims) { c.Ver = 0 }) },
			wantStatus: http.StatusUnauthorized, wantMsg: "token revoked", wantCode: "token_revoked",
		},
		{
			name:       "unknown user",
			token:      func() string { return f.mint(t, func(c *crypto.TokenClaims) { c.Sub = "user:" + meta.NewID() }) },
			wantStatus: http.StatusUnauthorized, wantMsg: "token revoked",
		},
		{
			name:       "project not in the snapshot",
			token:      func() string { return f.mint(t, func(c *crypto.TokenClaims) { c.Prj = meta.NewID() }) },
			wantStatus: http.StatusForbidden, wantMsg: "the token's project is not available",
		},
		{
			name:       "garbage bearer",
			token:      func() string { return "eyJ.not.atoken" },
			wantStatus: http.StatusUnauthorized, wantMsg: "invalid token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.stack(t).do(tc.token())
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body)
			}
			if got := authCode(t, w.Body.Bytes()); got != tc.wantMsg {
				t.Errorf("message = %q, want %q", got, tc.wantMsg)
			}
			if tc.wantCode != "" {
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.Error.Code != tc.wantCode {
					t.Errorf("code = %q, want %q", body.Error.Code, tc.wantCode)
				}
			}
		})
	}
}

// TestTokenPrincipal_DisabledProject: a disabled project leaves the snapshot,
// so its tokens stop resolving exactly like a deleted one's.
func TestTokenPrincipal_DisabledProject(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated")}
	disabled := false
	f.project.Spec.Enabled = &disabled

	w := f.stack(t).do(f.mint(t, nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
	}
}

// TestPrincipalFrom_Token proves a good token resolves to the same shape a
// key does, with the subject union the claims and the snapshot agree on.
func TestPrincipalFrom_Token(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated")}
	st := f.stack(t)

	token := f.mint(t, func(c *crypto.TokenClaims) { c.Grp = []string{"platform-eng"} })
	if w := st.do(token); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	p := st.seen
	if p == nil {
		t.Fatal("no principal on ctx")
	}
	if p.UserID != f.user || p.ServiceAccountID != "" {
		t.Errorf("principal ids = (%q, %q), want the token's user only", p.UserID, p.ServiceAccountID)
	}
	if p.ProjectID != f.project.Meta.ID || p.TeamID != f.team.Meta.ID {
		t.Errorf("tenancy = (%q, %q), want the claim's project and its team", p.ProjectID, p.TeamID)
	}
	if p.CredentialKind != CredentialToken || p.CredentialID == "" || p.KeyHash != "" {
		t.Errorf("credential = %+v, want a token with no key hash", p)
	}
	if p.Key != nil {
		t.Errorf("token principal carries a key row: %+v", p.Key)
	}
	if p.PassthroughAllowed {
		t.Error("a token must never be allowed to forward upstream keys")
	}
	// A token acts under its user's caller ladder.
	if p.Caller == nil || p.Caller.Principal != "user:"+f.user {
		t.Errorf("caller plan = %+v, want the user's", p.Caller)
	}
}

// A token whose jti was revoked is refused from the in-memory revoked set, before anything downstream runs.
func TestTokenPrincipal_RevokedJTI(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t)
	jti := meta.NewID()
	token := f.mint(t, func(c *crypto.TokenClaims) { c.Jti = jti })
	if w := st.do(token); w.Code != http.StatusOK {
		t.Fatalf("status before revoke = %d: %s", w.Code, w.Body)
	}
	if err := st.revoked.Revoke(context.Background(), jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	w := st.do(token)
	if w.Code != http.StatusUnauthorized || errCode(t, w.Body.Bytes()) != "token_revoked" {
		t.Fatalf("revoked token: %d %s, want 401 token_revoked", w.Code, w.Body)
	}
}

// TestTokenProxyMode: a token can never bring its own upstream key.
func TestTokenProxyMode_Forbidden(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated")}
	st := f.stack(t)

	r := proxyRequest(f.mint(t, nil))
	w := recordRequest(st, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
	}
}

// The caller ladder decides: the innermost rung's policy is the principal's, a disabled policy on any rung refuses with 403 and never falls through, and a principal with nothing bound is admitted (it reaches what its scopes own).
func TestCallerLadder(t *testing.T) {
	fls := false
	for _, tc := range []struct {
		name       string
		setup      func(*principalFixture)
		wantPolicy func(principalFixture) string
		wantStatus int
		wantCode   string
	}{
		{
			name: "the service account's binding is the innermost",
			setup: func(f *principalFixture) {
				f.bindings = []*policybinding.PolicyBinding{
					boundTo(*f, "b-project", 1, f.boundPol.Meta.ID, "project:"+f.project.Meta.ID),
					boundTo(*f, "b-sa", 1, f.keyPol.Meta.ID, "serviceaccount:"+f.sa.Meta.ID),
				}
			},
			wantPolicy: func(f principalFixture) string { return f.keyPol.Meta.ID },
			wantStatus: http.StatusOK,
		},
		{
			name:       "nothing bound is admitted with no caller policy",
			setup:      func(*principalFixture) {},
			wantStatus: http.StatusOK,
		},
		{
			name: "a disabled project policy refuses",
			setup: func(f *principalFixture) {
				f.boundPol.Spec.Enabled = &fls
				f.bindings = []*policybinding.PolicyBinding{
					boundTo(*f, "b-project", 1, f.boundPol.Meta.ID, "project:"+f.project.Meta.ID),
					boundTo(*f, "b-sa", 1, f.keyPol.Meta.ID, "serviceaccount:"+f.sa.Meta.ID),
				}
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "policy_disabled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrincipalFixture()
			tc.setup(&f)
			st := f.stack(t, saKey(f, "sk-wr-live"))
			rec := st.do("sk-wr-live")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantCode != "" {
				if got := errCode(t, rec.Body.Bytes()); got != tc.wantCode {
					t.Fatalf("code = %q, want %q", got, tc.wantCode)
				}
				return
			}
			got := st.seen.PolicyID()
			if tc.wantPolicy == nil {
				if got != "" {
					t.Errorf("policy = %q, want none", got)
				}
				return
			}
			if got != tc.wantPolicy(f) {
				t.Errorf("policy = %q, want %q", got, tc.wantPolicy(f))
			}
		})
	}
}

// subjectFrom parses a "<kind>:<id-or-name>" subject key back into the row
// shape a binding stores.
func subjectFrom(k string) rolebinding.Subject {
	kind, value, _ := strings.Cut(k, ":")
	s := rolebinding.Subject{Kind: rolebinding.SubjectKind(kind)}
	if s.Kind == rolebinding.SubjectGroup {
		s.Name = value
		return s
	}
	s.ID = value
	return s
}

// proxyRequest is a proxy-mode request carrying bearer as the relay
// credential and a caller-supplied upstream key.
func proxyRequest(bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(httpheader.HeaderProxyMode, "Proxy")
	r.Header.Set(httpheader.HeaderRelayAPIKey, bearer)
	r.Header.Set("Authorization", "Bearer sk-upstream")
	return r
}

func recordRequest(st *principalStack, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	st.handler.ServeHTTP(w, r)
	return w
}

// TestListModels_Token proves the model listing reads the principal's caller plan, whatever credential resolved it.
func TestListModels_Token(t *testing.T) {
	_, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, &Principal{
		CredentialKind: CredentialToken,
		CredentialID:   meta.NewID(),
		UserID:         pr.UserID,
		Caller:         pr.Caller,
	})

	out, err := listModels(ctx, "")
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(out.Body.Data) != 1 || out.Body.Data[0].ID != "test-model" {
		t.Errorf("models = %+v, want the one the policy grants", out.Body.Data)
	}
}

// boundTo builds a policy binding naming the given subject keys.
func boundTo(f principalFixture, name string, priority int, policyID string, subjects ...string) *policybinding.PolicyBinding {
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}},
		Spec: policybinding.Spec{ProjectID: f.project.Meta.ID, PolicyID: policyID, Priority: &priority},
	}
	for _, s := range subjects {
		b.Spec.Subjects = append(b.Spec.Subjects, subjectFrom(s))
	}
	return b
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// signedWith mints a token for the fixture's user under priv, naming kid.
func signedWith(t *testing.T, f principalFixture, priv ed25519.PrivateKey, kid string) string {
	t.Helper()
	return mintToken(t, priv, kid, f.user, f.project.Meta.ID, nil)
}

func TestSigningKeyRotationKeepsThePreviousKeyLive(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	oldPub := f.signer.Public().(ed25519.PublicKey)
	old := signedWith(t, f, f.signer, authtoken.KeyID(oldPub))

	newPub, newPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tokens.SetKey(newPub)
	fresh := signedWith(t, f, newPriv, authtoken.KeyID(newPub))

	st := f.stack(t)
	if w := st.do(fresh); w.Code != http.StatusOK {
		t.Fatalf("token under the new key: status %d: %s", w.Code, w.Body)
	}
	if w := st.do(old); w.Code != http.StatusOK {
		t.Fatalf("token under the previous key: status %d: %s — rotation must not be a global logout", w.Code, w.Body)
	}

	// A second rotation retires the original key.
	thirdPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tokens.SetKey(thirdPub)
	if w := st.do(old); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 once the key is two rotations old", w.Code)
	}
}

// Tokens minted before the kid header existed carry none and must keep
// verifying against the current key through the transition.
func TestTokenWithoutKidVerifiesAgainstTheCurrentKey(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	bare := signedWith(t, f, f.signer, "")
	if authtoken.HeaderKeyID(bare) != "" {
		t.Fatal("expected a token with no kid")
	}
	if w := f.stack(t).do(bare); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
}

// An at+jwt is an OAuth access token issued for some other resource. Signed
// with relay's own key and carrying valid claims it is still refused, while
// the same claims under a JWT header pass.
func TestAccessTokenTypIsRefused(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated")}
	pub := f.signer.Public().(ed25519.PublicKey)
	payload, err := json.Marshal(crypto.TokenClaims{
		Iss: crypto.TokenIssuer, Sub: "user:" + f.user, Prj: f.project.Meta.ID, Ver: 1,
		Jti: meta.NewID(), Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	signed := func(typ string) string {
		header := `{"alg":"EdDSA","typ":"` + typ + `","kid":"` + authtoken.KeyID(pub) + `"}`
		enc := base64.RawURLEncoding
		signing := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString(payload)
		return signing + "." + enc.EncodeToString(ed25519.Sign(f.signer, []byte(signing)))
	}

	st := f.stack(t)
	if w := st.do(signed(authtoken.TypeJWT)); w.Code != http.StatusOK {
		t.Fatalf("JWT header: status %d, want 200: %s", w.Code, w.Body)
	}
	w := st.do(signed(authtoken.TypeAccessToken))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("at+jwt header: status %d, want 401: %s", w.Code, w.Body)
	}
	if got := authCode(t, w.Body.Bytes()); got != "invalid token" {
		t.Errorf("message = %q, want invalid token", got)
	}
}

func TestUnknownKidIsRefused(t *testing.T) {
	f := newPrincipalFixture()
	tok := signedWith(t, f, f.signer, "deadbeefdeadbeef")
	if w := f.stack(t).do(tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a kid naming no held key", w.Code)
	}
}

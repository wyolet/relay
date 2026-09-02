package inference

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
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
	}{
		{
			name:       "another key's signature",
			token:      func() string { return signWith(t, otherPriv, f) },
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
			name:       "token version behind the user's",
			token:      func() string { return f.mint(t, func(c *crypto.TokenClaims) { c.Ver = 0 }) },
			wantStatus: http.StatusUnauthorized, wantMsg: "token revoked",
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
	// The claim's group and the snapshot's membership both land, sorted.
	want := []string{"user:" + f.user, "group:data-science", "group:platform-eng", "group:system:authenticated"}
	if !equalStrings(p.Subjects, want) {
		t.Errorf("subjects = %v, want %v", p.Subjects, want)
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

// TestPolicyResolution walks the resolution order: the credential's own
// policy, then the service account's, then the project's bindings by
// (priority, name), then the policy-less flow or a refusal.
func TestPolicyResolution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*principalFixture) *key.Key
		useToken   bool
		wantPolicy func(principalFixture) string
		wantStatus int
	}{
		{
			name: "the key's own policy wins over everything",
			setup: func(f *principalFixture) *key.Key {
				f.bindings = []*policybinding.PolicyBinding{boundTo(*f, "b", 1, f.boundPol.Meta.ID, "group:system:authenticated")}
				k := saKey(*f, "sk-wr-live")
				k.Spec.PolicyID = f.keyPol.Meta.ID
				return k
			},
			wantPolicy: func(f principalFixture) string { return f.keyPol.Meta.ID },
			wantStatus: http.StatusOK,
		},
		{
			name: "the service account's policy wins over a binding",
			setup: func(f *principalFixture) *key.Key {
				f.bindings = []*policybinding.PolicyBinding{boundTo(*f, "b", 1, f.boundPol.Meta.ID, "group:system:authenticated")}
				return saKey(*f, "sk-wr-live")
			},
			wantPolicy: func(f principalFixture) string { return f.saPol.Meta.ID },
			wantStatus: http.StatusOK,
		},
		{
			name: "a binding resolves when nothing overrides it",
			setup: func(f *principalFixture) *key.Key {
				f.sa.Spec.PolicyID = ""
				f.bindings = []*policybinding.PolicyBinding{
					boundTo(*f, "b-second", 20, f.keyPol.Meta.ID, "group:system:serviceaccounts"),
					boundTo(*f, "b-first", 10, f.boundPol.Meta.ID, "group:system:serviceaccounts"),
				}
				return saKey(*f, "sk-wr-live")
			},
			wantPolicy: func(f principalFixture) string { return f.boundPol.Meta.ID },
			wantStatus: http.StatusOK,
		},
		{
			name: "a binding whose subjects don't intersect is skipped",
			setup: func(f *principalFixture) *key.Key {
				f.sa.Spec.PolicyID = ""
				f.bindings = []*policybinding.PolicyBinding{
					boundTo(*f, "b-other", 1, f.keyPol.Meta.ID, "serviceaccount:"+meta.NewID()),
					boundTo(*f, "b-mine", 5, f.boundPol.Meta.ID, "serviceaccount:"+f.sa.Meta.ID),
				}
				return saKey(*f, "sk-wr-live")
			},
			wantPolicy: func(f principalFixture) string { return f.boundPol.Meta.ID },
			wantStatus: http.StatusOK,
		},
		{
			name: "a project-scoped key with nothing bound is refused",
			setup: func(f *principalFixture) *key.Key {
				f.sa.Spec.PolicyID = ""
				return saKey(*f, "sk-wr-live")
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "a personal key with no project takes the policy-less flow",
			setup: func(f *principalFixture) *key.Key {
				return &key.Key{
					Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-personal", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
					Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user}, KeyHash: sha("sk-wr-live")},
				}
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a token with nothing bound is refused — it never goes policy-less",
			setup: func(f *principalFixture) *key.Key {
				f.bindings = nil
				return nil
			},
			useToken:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name: "a token resolves through a group subject",
			setup: func(f *principalFixture) *key.Key {
				f.bindings = []*policybinding.PolicyBinding{boundTo(*f, "b", 1, f.boundPol.Meta.ID, "group:data-science")}
				return nil
			},
			useToken:   true,
			wantPolicy: func(f principalFixture) string { return f.boundPol.Meta.ID },
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrincipalFixture()
			k := tc.setup(&f)

			var st *principalStack
			bearer := "sk-wr-live"
			if tc.useToken {
				st = f.stack(t)
				bearer = f.mint(t, nil)
			} else {
				st = f.stack(t, k)
			}
			rec := st.do(bearer)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantStatus != http.StatusOK {
				if got := authCode(t, rec.Body.Bytes()); got != "no policy is bound to this principal" {
					t.Errorf("message = %q, want the no_policy refusal", got)
				}
				return
			}
			if tc.wantPolicy == nil {
				if st.seen.Policy != nil {
					t.Errorf("policy = %q, want none (policy-less flow)", st.seen.Policy.Meta.ID)
				}
				return
			}
			if st.seen.Policy == nil || st.seen.Policy.Meta.ID != tc.wantPolicy(f) {
				t.Errorf("policy = %+v, want %q", st.seen.Policy, tc.wantPolicy(f))
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

// TestListModels_Token proves the model listing reads the principal's
// resolved policy, whatever credential resolved it.
func TestListModels_Token(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, &Principal{
		CredentialKind: CredentialToken,
		CredentialID:   meta.NewID(),
		UserID:         meta.NewID(),
		ProjectID:      meta.NewID(),
		Policy:         pr.Policy,
	})

	out, err := listModels(ctx, buildDeps(t, cat), "")
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
		Spec: policybinding.Spec{ProjectID: f.project.Meta.ID, PolicyID: policyID, Priority: priority},
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

func signWith(t *testing.T, priv ed25519.PrivateKey, f principalFixture) string {
	t.Helper()
	token, err := crypto.SignToken(priv, crypto.TokenClaims{
		Iss: crypto.TokenIssuer, Sub: "user:" + f.user, Prj: f.project.Meta.ID,
		Ver: 1, Jti: meta.NewID(), Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

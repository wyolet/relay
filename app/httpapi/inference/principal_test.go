package inference

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/crypto"
)

// principalStack wires a catalog holding one team → project → service
// account plus the supplied keys and groups, and returns a handler that
// records the principal the middleware resolved.
type principalStack struct {
	cat     *catalog.Catalog
	handler http.Handler
	seen    *Principal
}

type principalFixture struct {
	team    *team.Team
	project *project.Project
	sa      *serviceaccount.ServiceAccount
	group   *group.Group
	user    string

	// saPol is the service account's own policy; the others exist so a test
	// can attach them to a key or a binding and pin the resolution order.
	saPol, keyPol, boundPol *policy.Policy
	bindings                []*policybinding.PolicyBinding
	versions                stubVersions

	tokens *TokenVerifier
	signer ed25519.PrivateKey
}

// stubVersions is the catalog's token-version source in tests.
type stubVersions map[string]int

func (v stubVersions) TokenVersions(context.Context) (map[string]int, error) { return v, nil }

func testPolicy(name string, projectID string) *policy.Policy {
	return &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}},
	}
}

func newPrincipalFixture() principalFixture {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	sa := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"},
		Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID},
	}
	sa.StampOwner()
	user := meta.NewID()
	g := &group.Group{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: group.Spec{MemberIDs: []string{user}},
	}
	saPol := testPolicy("sa-pol", proj.Meta.ID)
	sa.Spec.PolicyID = saPol.Meta.ID
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	verifier := &TokenVerifier{}
	verifier.SetKey(pub)
	return principalFixture{
		team: tm, project: proj, sa: sa, group: g, user: user,
		saPol:    saPol,
		keyPol:   testPolicy("key-pol", proj.Meta.ID),
		boundPol: testPolicy("bound-pol", proj.Meta.ID),
		versions: stubVersions{user: 1},
		tokens:   verifier,
		signer:   priv,
	}
}

// mint signs a token for this fixture's user + project with the claims a
// mint would produce, letting a test mutate them first.
func (f principalFixture) mint(t testing.TB, mutate func(*crypto.TokenClaims)) string {
	t.Helper()
	return mintToken(t, f.signer, "", f.user, f.project.Meta.ID, mutate)
}

func (f principalFixture) stack(t testing.TB, keys ...*key.Key) *principalStack {
	t.Helper()
	c := catalogtest.Catalog{
		Policies:        []*policy.Policy{f.saPol, f.keyPol, f.boundPol},
		Keys:            keys,
		Teams:           []*team.Team{f.team},
		Projects:        []*project.Project{f.project},
		ServiceAccounts: []*serviceaccount.ServiceAccount{f.sa},
		Groups:          []*group.Group{f.group},
		PolicyBindings:  f.bindings,
	}.New()
	c.UseTokenVersions(f.versions)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	st := &principalStack{cat: c}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.seen = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	st.handler = ClassifyMiddleware()(PrincipalMiddleware(c, f.tokens)(inner))
	return st
}

func (st *principalStack) do(plaintext string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	w := httptest.NewRecorder()
	st.handler.ServeHTTP(w, r)
	return w
}

func sha(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func authCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error.Message
}

func saKey(f principalFixture, plaintext string) *key.Key {
	return &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: f.sa.Meta.ID},
			KeyHash:   sha(plaintext),
		},
	}
}

func TestPrincipalMiddleware_Rejections(t *testing.T) {
	f := newPrincipalFixture()
	past := time.Now().Add(-time.Hour)
	fls := false

	for _, tc := range []struct {
		name   string
		mutate func(*key.Key)
		want   string
	}{
		// A disabled key never reaches the snapshot (build reads enabled
		// rows only), so it is rejected as unknown rather than as disabled.
		{name: "disabled", mutate: func(k *key.Key) { k.Spec.Enabled = &fls }, want: "invalid api key"},
		{name: "revoked", mutate: func(k *key.Key) { k.Spec.RevokedAt = &past }, want: "api key revoked"},
		{name: "expired", mutate: func(k *key.Key) { k.Spec.ExpiresAt = &past }, want: "api key expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := saKey(f, "sk-wr-live")
			tc.mutate(k)
			w := f.stack(t, k).do("sk-wr-live")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if got := authCode(t, w.Body.Bytes()); got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("unknown key", func(t *testing.T) {
		w := f.stack(t, saKey(f, "sk-wr-live")).do("sk-wr-other")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := authCode(t, w.Body.Bytes()); got != "invalid api key" {
			t.Errorf("message = %q, want %q", got, "invalid api key")
		}
	})
}

func TestPrincipalMiddleware_RotationGrace(t *testing.T) {
	f := newPrincipalFixture()

	t.Run("old plaintext accepted inside the window", func(t *testing.T) {
		k := saKey(f, "sk-wr-new")
		until := time.Now().Add(time.Hour)
		k.Spec.PreviousKeyHash = sha("sk-wr-old")
		k.Spec.GraceUntil = &until
		st := f.stack(t, k)
		if w := st.do("sk-wr-old"); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
		}
		if w := st.do("sk-wr-new"); w.Code != http.StatusOK {
			t.Fatalf("new plaintext status = %d, want 200", w.Code)
		}
	})

	t.Run("old plaintext rejected once the window closes", func(t *testing.T) {
		k := saKey(f, "sk-wr-new")
		// The snapshot indexes the previous hash because the window is open
		// at build time; it closes before the request, with no reconcile in
		// between, so the middleware is what has to reject it.
		until := time.Now().Add(30 * time.Millisecond)
		k.Spec.PreviousKeyHash = sha("sk-wr-old")
		k.Spec.GraceUntil = &until
		st := f.stack(t, k)
		time.Sleep(60 * time.Millisecond)

		w := st.do("sk-wr-old")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := authCode(t, w.Body.Bytes()); got != "api key rotated" {
			t.Errorf("message = %q, want %q", got, "api key rotated")
		}
	})
}

func TestPrincipalFrom_ServiceAccountKey(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-wr-live"))
	if w := st.do("sk-wr-live"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	p := st.seen
	if p == nil {
		t.Fatal("no principal on ctx")
	}
	if p.ServiceAccountID != f.sa.Meta.ID || p.UserID != "" {
		t.Errorf("principal ids = (%q, %q), want service account only", p.ServiceAccountID, p.UserID)
	}
	if p.ProjectID != f.project.Meta.ID || p.TeamID != f.team.Meta.ID {
		t.Errorf("tenancy = (%q, %q), want (%q, %q)", p.ProjectID, p.TeamID, f.project.Meta.ID, f.team.Meta.ID)
	}
	if p.CredentialKind != "key" || p.CredentialID == "" || p.KeyHash != sha("sk-wr-live") {
		t.Errorf("credential = %+v", p)
	}
	if p.Key == nil || p.Key.Meta.ID != p.CredentialID {
		t.Errorf("key = %+v, want the credential row", p.Key)
	}
}

func TestPrincipalFrom_UserKey(t *testing.T) {
	f := newPrincipalFixture()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-personal", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user},
			KeyHash:   sha("sk-wr-personal"),
		},
	}
	st := f.stack(t, k)
	if w := st.do("sk-wr-personal"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	p := st.seen
	if p == nil {
		t.Fatal("no principal on ctx")
	}
	if p.UserID != f.user || p.ServiceAccountID != "" {
		t.Errorf("principal ids = (%q, %q), want user only", p.UserID, p.ServiceAccountID)
	}
	if p.ProjectID != "" || p.TeamID != "" {
		t.Errorf("personal key should carry no tenancy, got (%q, %q)", p.ProjectID, p.TeamID)
	}
	if p.CredentialKind != "key" || p.KeyHash != sha("sk-wr-personal") {
		t.Errorf("credential = %+v", p)
	}
}

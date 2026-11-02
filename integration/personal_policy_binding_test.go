//go:build integration

// personal_policy_binding_test.go drives personal policies through the control API under RELAY_AUTHZ=rbac and spends keys and tokens on the data plane: a user binds their own policy to themselves for their relay keys while a project binds them for its tokens, a project admin binds theirs to the project's service account, and the binding stops applying when the admin loses the project.

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/ids"
)

func TestIntegration_PersonalPolicyBindings(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, tokenUpstreamResponse)
	}))
	t.Cleanup(upstream.Close)
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	st.seedHappyPath(upstream.URL, "sk-mock-upstream-key")
	f := &tokenFixture{stack: st, project: "personal-project"}

	teamID := st.mkTeam(t, "personal-team")
	projectID := st.mkProject(t, f.project, teamID)
	saID := st.mkServiceAccount(t, "personal-sa", projectID)
	aliceID := st.seedLogin(t, "personal-alice", "pw-personal-alice")
	bobID := st.seedLogin(t, "personal-bob", "pw-personal-bob")
	code, raw := st.adminDo(http.MethodPost, "/api/role-bindings", `{"metadata":{"name":"personal-alice-admin","owner":{"kind":"project","id":"`+projectID+`"}},`+
		`"spec":{"roleId":"`+roles["project-admin"].Meta.ID+`","scope":{"kind":"project","id":"`+projectID+`"},"subjects":[{"kind":"user","id":"`+aliceID+`"}]}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/role-bindings = %d: %s", code, raw)
	}
	aliceAdmin := decodeID(t, raw)
	seedKey := func(name, plaintext string, principal key.Principal, owner meta.Owner) string {
		mustUpsert(t, st.stores.Key.Upsert(ctx, &key.Key{
			Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner},
			Spec: key.Spec{Principal: principal, KeyHash: sha256Hex(plaintext), Prefix: "rk_test"},
		}), "key")
		return plaintext
	}
	aliceKey := seedKey("personal-alice-key", "rk_test_personal_alice", key.Principal{Kind: key.PrincipalUser, ID: aliceID}, meta.Owner{Kind: meta.OwnerUser, ID: aliceID})
	saKey := seedKey("personal-sa-key", "rk_test_personal_sa", key.Principal{Kind: key.PrincipalServiceAccount, ID: saID}, meta.Owner{Kind: meta.OwnerProject, ID: projectID})
	// A second model at the happy path's host, the only one the personal policies grant.
	provs, err := st.stores.Provider.List(ctx)
	if err != nil || len(provs) != 1 {
		t.Fatalf("list providers: %d, %v", len(provs), err)
	}
	hosts, err := st.stores.Host.List(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("list hosts: %d, %v", len(hosts), err)
	}
	elsewhere := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "elsewhere", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provs[0].Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "elsewhere"}}, Pointer: "elsewhere"},
	}
	mustUpsert(t, st.stores.Model.Upsert(ctx, elsewhere), "model")
	mustUpsert(t, st.stores.Binding.Upsert(ctx, &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "elsewhere-on-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: elsewhere.Meta.ID, HostID: hosts[0].Meta.ID, Adapter: adapters.OpenAI},
	}), "binding")
	if err := st.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	alice := st.login(t, "personal-alice", "pw-personal-alice")
	bob := st.login(t, "personal-bob", "pw-personal-bob")

	// A personal policy granting a model the happy path does not serve: whatever it governs is refused test-model.
	mkPolicy := func(as func(method, path, body string) (int, []byte), name string) string {
		code, raw := as(http.MethodPost, "/api/policies", `{"metadata":{"name":"`+name+`"},"spec":{"models":["test-provider/elsewhere"]}}`)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/policies %s = %d: %s", name, code, raw)
		}
		return decodeID(t, raw)
	}
	alicesPolicy := mkPolicy(alice.do, "personal-alice-policy")
	// The happy path's system policy grants test-model alone.
	code, raw = st.adminDo(http.MethodGet, "/api/policies/test-policy", "")
	if code != http.StatusOK {
		t.Fatalf("GET test-policy = %d: %s", code, raw)
	}
	sharedPolicy := decodeID(t, raw)
	bobsPolicy := mkPolicy(bob.do, "personal-bob-policy")
	binding := func(name, owner, policyID, accountID string) string {
		return `{"metadata":{"name":"` + name + `","owner":` + owner + `},"spec":{"policyId":"` + policyID + `","accountIds":["` + accountID + `"]}}`
	}
	ownedBy := func(userID string) string { return `{"kind":"user","id":"` + userID + `"}` }
	personal := func(userID string) string { return st.accountAt(t, "personal:"+userID) }
	aliceInProject := st.accountAt(t, "member:project:"+projectID+":"+aliceID)
	project := `{"kind":"project","id":"` + projectID + `"}`
	sa := st.accountAt(t, "service:"+saID)

	if got, reason := f.chat(aliceKey, "test-model"); got != http.StatusOK {
		t.Fatalf("alice's key before any binding = %d (%s)", got, reason)
	}

	t.Run("a user binds their own policy to their personal account", func(t *testing.T) {
		code, raw := alice.do(http.MethodPost, "/api/policy-bindings", binding("personal-alice-own", `{"kind":"user"}`, alicesPolicy, personal(aliceID)))
		if code != http.StatusCreated {
			t.Fatalf("POST = %d: %s", code, raw)
		}
		if err := st.reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got, reason := f.chat(aliceKey, "test-model"); got != http.StatusForbidden || reason != "model_not_allowed" {
			t.Fatalf("alice's key under her own policy = %d (%s), want 403 model_not_allowed", got, reason)
		}
		code, token, raw := f.mint(alice)
		if code != http.StatusOK {
			t.Fatalf("mint = %d: %s", code, raw)
		}
		if got, reason := f.chat(token, "test-model"); got != http.StatusOK {
			t.Fatalf("alice's token, which acts inside a project her own binding does not govern = %d (%s), want 200", got, reason)
		}
	})

	t.Run("a project binds the user's member account beside her personal one", func(t *testing.T) {
		code, raw := st.adminDo(http.MethodPost, "/api/policy-bindings", binding("personal-alice-in-project", project, sharedPolicy, aliceInProject))
		if code != http.StatusCreated {
			t.Fatalf("POST = %d: %s", code, raw)
		}
		if err := st.reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		_, token, raw := f.mint(alice)
		for _, c := range []struct {
			who, bearer, model string
			want               int
		}{
			{"key", aliceKey, "test-model", http.StatusForbidden},
			{"key", aliceKey, "elsewhere", http.StatusOK},
			{"token", token, "test-model", http.StatusOK},
			{"token", token, "elsewhere", http.StatusForbidden},
		} {
			if got, reason := f.chat(c.bearer, c.model); got != c.want {
				t.Errorf("alice's %s for %s = %d (%s), want %d: the key follows her own policy, the token the project's (%s)", c.who, c.model, got, reason, c.want, raw)
			}
		}
	})

	for _, tc := range []struct {
		name string
		as   func(method, path, body string) (int, []byte)
		body string
		want int
		says string
	}{
		{"a user cannot see another user's account to bind it", alice.do, binding("personal-alice-on-bob", ownedBy(aliceID), alicesPolicy, personal(bobID)), http.StatusNotFound, "not found"},
		{"the operator binds a user's policy to another user's account", st.adminDo, binding("personal-alice-on-bob-admin", ownedBy(aliceID), alicesPolicy, personal(bobID)), http.StatusBadRequest, "outside the scope"},
		{"the operator binds one user's policy to another user's own binding", st.adminDo, binding("personal-bob-with-alices", ownedBy(bobID), alicesPolicy, personal(bobID)), http.StatusBadRequest, "cannot use it"},
		{"a user cannot see another user's policy to bind it", bob.do, binding("personal-bob-borrows", `{"kind":"user"}`, alicesPolicy, personal(bobID)), http.StatusNotFound, "not found"},
		{"a project binds a policy of someone who may not manage its bindings", st.adminDo, binding("personal-bob-on-sa", project, bobsPolicy, sa), http.StatusBadRequest, "may not manage policy bindings"},
		{"the system binds an account its user already bound", st.adminDo, binding("personal-alice-everywhere", `{"kind":"system"}`, sharedPolicy, personal(aliceID)), http.StatusConflict, "duplicate_binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, raw := tc.as(http.MethodPost, "/api/policy-bindings", tc.body); code != tc.want || !strings.Contains(string(raw), tc.says) {
				t.Fatalf("POST = %d, want %d saying %q: %s", code, tc.want, tc.says, raw)
			}
		})
	}

	var saBinding string
	t.Run("a project admin binds their own policy to the project's service account", func(t *testing.T) {
		code, raw := alice.do(http.MethodPost, "/api/policy-bindings", binding("personal-alice-on-sa", project, alicesPolicy, sa))
		if code != http.StatusCreated {
			t.Fatalf("POST = %d: %s", code, raw)
		}
		saBinding = decodeID(t, raw)
		if err := st.reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got, reason := f.chat(saKey, "test-model"); got != http.StatusForbidden || reason != "model_not_allowed" {
			t.Fatalf("service account key under alice's policy = %d (%s), want 403 model_not_allowed", got, reason)
		}
	})

	t.Run("the binding stops applying when its owner leaves the project", func(t *testing.T) {
		if saBinding == "" {
			t.Skip("the service account binding was not created")
		}
		if code, raw := st.adminDo(http.MethodDelete, "/api/role-bindings/by-id/"+aliceAdmin, ""); code != http.StatusNoContent && code != http.StatusOK {
			t.Fatalf("DELETE role binding = %d: %s", code, raw)
		}
		if err := st.reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got, reason := f.chat(saKey, "test-model"); got != http.StatusOK {
			t.Fatalf("service account key after alice lost the project = %d (%s), want 200", got, reason)
		}
		found := false
		for _, is := range st.engine.View().Issues(engine.Ref{Kind: plan.KindPolicyBinding, ID: saBinding}) {
			found = found || is.Code == plan.IssuePolicyNotBindable
		}
		if !found {
			t.Fatalf("issues on the binding = %+v, want %s", st.engine.View().Issues(engine.Ref{Kind: plan.KindPolicyBinding, ID: saBinding}), plan.IssuePolicyNotBindable)
		}
	})
}

func decodeID(t *testing.T, raw []byte) string {
	t.Helper()
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil || row.Metadata.ID == "" {
		t.Fatalf("decode row id: %v: %s", err, raw)
	}
	return row.Metadata.ID
}

//go:build integration

// policy_delete_integration_test.go deletes policies through the real handler
// against Postgres: the referencing rows live in concrete stores, and what
// matters is that a refused delete leaves every one of them untouched.
// Run with: make test-integration.

package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
)

// policyDeleteFixture is a project with a service account, a host, and a
// user outside the project, plus the mounted control API.
type policyDeleteFixture struct {
	pool    *pgxpool.Pool
	cat     *appcatalog.Catalog
	stores  *appcatalog.Stores
	users   *user.Store
	handler http.Handler
	sink    *auditSink
	emitter *audit.Emitter
	proj    *project.Project
	account *serviceaccount.ServiceAccount
	host    *host.Host
	alice   *user.User
}

// inUseResponse mirrors the 409 body a client reads.
type inUseResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Blockers []struct {
		Kind       string `json:"kind"`
		Field      string `json:"field"`
		Detachable bool   `json:"detachable"`
		Count      int    `json:"count"`
		Hidden     int    `json:"hidden"`
		Items      []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	} `json:"blockers"`
}

func newPolicyDeleteFixture(t *testing.T) (policyDeleteFixture, context.Context) {
	t.Helper()
	pool, ctx := setupPolicyRefDB(t)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	f := policyDeleteFixture{pool: pool, cat: cat, stores: stores, users: user.NewStore(gen.New(pool)), sink: &auditSink{}}

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "del-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Team.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	f.proj = &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "del-project"}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	f.proj.StampOwner()
	if err := stores.Project.Upsert(ctx, f.proj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	f.account = &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "del-account"},
		Spec: serviceaccount.Spec{ProjectID: f.proj.Meta.ID},
	}
	f.account.StampOwner()
	if err := stores.ServiceAccount.Upsert(ctx, f.account); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}
	f.host = &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "del-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, f.host); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	f.alice = &user.User{ID: meta.NewID(), Username: "del-alice-" + meta.NewID()[:8]}
	if err := f.users.Upsert(ctx, f.alice); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	deps := mountDeps(t)
	deps.Stores = stores
	deps.Users = f.users
	deps.Catalog = cat
	deps.Authz = audit.Authorizer{Inner: testRBAC()}
	f.emitter = audit.NewEmitter(f.sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps.Audit = f.emitter
	r := chi.NewRouter()
	// X-Test-User signs the request in as alice, a user with no binding in
	// the project.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("X-Test-User") == "alice" {
				req = req.WithContext(actor.WithActor(req.Context(), &actor.Actor{UserID: f.alice.ID, Username: f.alice.Username}))
			}
			next.ServeHTTP(w, req)
		})
	})
	Mount(r, deps)
	f.handler = r
	return f, ctx
}

func (f policyDeleteFixture) policy(t *testing.T, ctx context.Context, name string, owner meta.Owner) *policy.Policy {
	t.Helper()
	p := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner}}
	if err := f.stores.Policy.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert policy %s: %v", name, err)
	}
	return p
}

func (f policyDeleteFixture) projectKey(t *testing.T, ctx context.Context, name, policyID string) *key.Key {
	t.Helper()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: f.account.Meta.ID},
			PolicyID:  policyID,
			KeyHash:   strings.Repeat(name[len(name)-1:], 64), Prefix: "sk-" + name,
		},
	}
	if err := f.stores.Key.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert key %s: %v", name, err)
	}
	return k
}

func (f policyDeleteFixture) deletePolicy(t *testing.T, id string, asAlice bool) *httptest.ResponseRecorder {
	t.Helper()
	return f.deleteRow(t, "policies", id, asAlice)
}

// deleteRow sends DELETE as the admin token, or as alice when asAlice.
func (f policyDeleteFixture) deleteRow(t *testing.T, plural, id string, asAlice bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/"+plural+"/by-id/"+id, nil)
	if asAlice {
		req.Header.Set("X-Test-User", "alice")
	} else {
		req.Header.Set("Authorization", "Bearer test-admin-token")
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func decodeInUse(t *testing.T, w *httptest.ResponseRecorder) inUseResponse {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var body inUseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v: %s", err, w.Body.String())
	}
	if body.Error.Code != "resource_in_use" {
		t.Fatalf("error.code = %q, want resource_in_use", body.Error.Code)
	}
	return body
}

func (f policyDeleteFixture) policyExists(t *testing.T, ctx context.Context, id string) bool {
	t.Helper()
	p, err := f.stores.Policy.Get(ctx, id)
	return err == nil && p != nil
}

func TestDeletePolicy_ReferencedByKeyIsRefusedAsDetachable(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "used-by-key", meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})
	k := f.projectKey(t, ctx, "key-a", pol.Meta.ID)

	body := decodeInUse(t, f.deletePolicy(t, pol.Meta.ID, false))
	if len(body.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want one group", body.Blockers)
	}
	g := body.Blockers[0]
	if g.Kind != "key" || g.Field != "spec.policyId" || !g.Detachable || g.Count != 1 || g.Hidden != 0 {
		t.Fatalf("group = %+v, want one detachable key via spec.policyId", g)
	}
	if len(g.Items) != 1 || g.Items[0].ID != k.Meta.ID || g.Items[0].Name != k.Meta.Name {
		t.Fatalf("items = %+v, want key %q", g.Items, k.Meta.Name)
	}

	if !f.policyExists(t, ctx, pol.Meta.ID) {
		t.Fatal("the refused delete removed the policy")
	}
	got, err := f.stores.Key.Get(ctx, k.Meta.ID)
	if err != nil || got == nil || got.Spec.PolicyID != pol.Meta.ID {
		t.Fatalf("key after refused delete = %+v (err %v), want it still on the policy", got, err)
	}

	f.emitter.Close()
	var found bool
	for _, ev := range f.sink.all() {
		if ev.Action == "policies.delete" && ev.Resource.ID == pol.Meta.ID {
			found = true
			if ev.Outcome.Code != http.StatusConflict || ev.Change != nil {
				t.Fatalf("audit event = %+v, want code 409 and no change", ev)
			}
		}
	}
	if !found {
		t.Fatal("the refused delete left no audit row")
	}
}

func TestDeletePolicy_TierOfHostKeyIsRefusedNotDetachable(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	t.Setenv("DEL_HOSTKEY_VALUE", "sk-test-value")
	pol := f.policy(t, ctx, "tier-of-host-key", meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-host-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: f.host.Meta.ID, PolicyID: pol.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "DEL_HOSTKEY_VALUE"}},
	}
	if err := f.stores.HostKey.Upsert(ctx, hk); err != nil {
		t.Fatalf("upsert host key: %v", err)
	}

	body := decodeInUse(t, f.deletePolicy(t, pol.Meta.ID, false))
	if len(body.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want one group", body.Blockers)
	}
	g := body.Blockers[0]
	if g.Kind != "host-key" || g.Field != "spec.policyId" || g.Detachable || g.Count != 1 {
		t.Fatalf("group = %+v, want one non-detachable host-key via spec.policyId", g)
	}
	got, err := f.stores.HostKey.Get(ctx, hk.Meta.ID)
	if err != nil || got == nil || got.Spec.PolicyID != pol.Meta.ID {
		t.Fatalf("host key after refused delete = %+v (err %v), want it still on the policy", got, err)
	}
}

func TestDeletePolicy_BoundByPolicyBindingIsRefusedNotDetachable(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "bound-policy", meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})
	pb := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "del-binding"},
		Spec: policybinding.Spec{
			ProjectID: f.proj.Meta.ID, PolicyID: pol.Meta.ID,
			Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID}},
		},
	}
	pb.StampOwner()
	if err := f.stores.PolicyBinding.Upsert(ctx, pb); err != nil {
		t.Fatalf("upsert policy binding: %v", err)
	}

	body := decodeInUse(t, f.deletePolicy(t, pol.Meta.ID, false))
	if len(body.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want one group", body.Blockers)
	}
	g := body.Blockers[0]
	if g.Kind != "policy-binding" || g.Field != "spec.policyId" || g.Detachable || g.Count != 1 {
		t.Fatalf("group = %+v, want one non-detachable policy-binding via spec.policyId", g)
	}
	// The binding's FK cascades on a policy delete, so a delete that slipped
	// through would have taken it along.
	if got, err := f.stores.PolicyBinding.Get(ctx, pb.Meta.ID); err != nil || got == nil {
		t.Fatalf("policy binding after refused delete: %v", err)
	}
}

func TestDeletePolicy_ServiceAccountOverrideIsRefusedAsDetachable(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "account-override", meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})
	f.account.Spec.PolicyID = pol.Meta.ID
	if err := f.stores.ServiceAccount.Upsert(ctx, f.account); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}

	body := decodeInUse(t, f.deletePolicy(t, pol.Meta.ID, false))
	if len(body.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want one group", body.Blockers)
	}
	g := body.Blockers[0]
	if g.Kind != "service-account" || g.Field != "spec.policyId" || !g.Detachable || g.Count != 1 {
		t.Fatalf("group = %+v, want one detachable service-account via spec.policyId", g)
	}
	got, err := f.stores.ServiceAccount.Get(ctx, f.account.Meta.ID)
	if err != nil || got == nil || got.Spec.PolicyID != pol.Meta.ID {
		t.Fatalf("service account after refused delete = %+v (err %v), want it still on the policy", got, err)
	}
}

// A referencing row the caller may not see still blocks, but is reported as
// a count only.
func TestDeletePolicy_HiddenReferenceBlocksWithoutItsName(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "alice-policy", meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID})
	hidden := f.projectKey(t, ctx, "key-b", pol.Meta.ID)
	own := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: f.alice.ID},
			PolicyID:  pol.Meta.ID,
			KeyHash:   strings.Repeat("c", 64), Prefix: "sk-alice",
		},
	}
	if err := f.stores.Key.Upsert(ctx, own); err != nil {
		t.Fatalf("upsert key: %v", err)
	}

	w := f.deletePolicy(t, pol.Meta.ID, true)
	body := decodeInUse(t, w)
	if strings.Contains(w.Body.String(), hidden.Meta.Name) || strings.Contains(w.Body.String(), hidden.Meta.ID) {
		t.Fatalf("409 body names a row the caller may not see: %s", w.Body.String())
	}
	if len(body.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want one group", body.Blockers)
	}
	g := body.Blockers[0]
	if g.Kind != "key" || g.Count != 2 || g.Hidden != 1 {
		t.Fatalf("group = %+v, want two keys, one of them hidden", g)
	}
	if len(g.Items) != 1 || g.Items[0].ID != own.Meta.ID {
		t.Fatalf("items = %+v, want only alice's own key", g.Items)
	}
	if !f.policyExists(t, ctx, pol.Meta.ID) {
		t.Fatal("the refused delete removed the policy")
	}
}

func TestDeletePolicy_UnreferencedIsDeleted(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "unused-policy", meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})

	if w := f.deletePolicy(t, pol.Meta.ID, false); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
	}
	if f.policyExists(t, ctx, pol.Meta.ID) {
		t.Fatal("the policy is still stored")
	}
}

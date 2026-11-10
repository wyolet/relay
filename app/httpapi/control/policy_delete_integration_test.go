//go:build integration

// policy_delete_integration_test.go deletes policies through the real handler
// against Postgres: the referencing rows live in concrete stores, and what
// matters is that a refused delete leaves every one of them untouched.
// Run with: make test-integration.

package control

import (
	"context"
	"encoding/json"
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
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
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
	f := policyDeleteFixture{pool: pool, cat: cat, stores: stores, users: user.NewStore(pool), sink: &auditSink{}}

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
	f.emitter = audit.NewEmitter(f.sink, slog.New(slog.DiscardHandler))
	deps.Audit = f.emitter
	r := chi.NewRouter()
	// X-Test-User signs the request in as alice, a user with no binding in
	// the project.
	r.Use(withTestActor("X-Test-User", map[string]*actor.Actor{"alice": {UserID: f.alice.ID, Username: f.alice.Username}}))
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

func (f policyDeleteFixture) projectKey(t *testing.T, ctx context.Context, name string) *key.Key {
	t.Helper()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: f.account.Meta.ID},
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

// A host's tier policy that nothing binds deletes: the host itself holds no reference to it.
func TestDeletePolicy_UnboundHostTierDeletes(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "unbound-tier", meta.Owner{Kind: meta.OwnerHost, ID: f.host.Meta.ID})
	f.allowCatalogDeletes(t, ctx, "governance:policy")

	wantStatus(t, f.deletePolicy(t, pol.Meta.ID, false), http.StatusNoContent)
	if f.policyExists(t, ctx, pol.Meta.ID) {
		t.Fatal("the policy is still stored")
	}
}

func TestDeletePolicy_BoundByPolicyBindingIsRefusedNotDetachable(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "bound-policy", meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID})
	pb := f.policyBinding(t, ctx, "del-binding", pol.Meta.ID, f.accountOf(t, ctx, f.account))

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

// A referencing row the caller may not see still blocks, but is reported as
// a count only.
func TestDeletePolicy_HiddenReferenceBlocksWithoutItsName(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "alice-policy", meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID})
	hidden := f.policyBinding(t, ctx, "hidden-binding", pol.Meta.ID, f.accountOf(t, ctx, f.account))

	w := f.deletePolicy(t, pol.Meta.ID, true)
	body := decodeInUse(t, w)
	if strings.Contains(w.Body.String(), hidden.Meta.Name) || strings.Contains(w.Body.String(), hidden.Meta.ID) {
		t.Fatalf("409 body names a row the caller may not see: %s", w.Body.String())
	}
	if len(body.Blockers) != 1 {
		t.Fatalf("blockers = %+v, want one group", body.Blockers)
	}
	g := body.Blockers[0]
	if g.Kind != "policy-binding" || g.Count != 1 || g.Hidden != 1 || len(g.Items) != 0 {
		t.Fatalf("group = %+v, want one hidden binding and no items", g)
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

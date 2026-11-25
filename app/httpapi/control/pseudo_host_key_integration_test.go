//go:build integration

package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// Key values the upstream answers, each distinctive enough to grep for in answers, logs and audit rows.
const (
	keyOrgA    = "sk-hkform-secret-org-a-7c1f9e2b4d6a8c0e"
	keyOrgA2   = "sk-hkform-secret-org-a-second-3e5a7c9b"
	keyOrgB    = "sk-hkform-secret-org-b-9d2e4f6a8b0c1d3e"
	keyOrgC    = "sk-hkform-secret-org-c-1a3c5e7f9b2d4f6a"
	keyBare    = "sk-hkform-secret-bare-5b7d9f1a3c5e7a9c"
	keyBusy    = "sk-hkform-secret-busy-8e0a2c4e6a8c0e2a"
	keyRevoked = "sk-hkform-secret-revoked-2f4b6d8f0b2d"
)

// hostKeyFormWorld is the control API in roles mode over Postgres with a master key: host alpha answers organization probes (with its free and paid tier presets), host beta has a tier preset of its own, and host local takes no key. Alice owns two provider accounts at alpha, work (org-a, bound to the paid tier) and other (org-b); bob owns one there. Bob administers team two, where alice is a viewer; team two holds project two-app. Carol holds no role.
type hostKeyFormWorld struct {
	r                     http.Handler
	pool                  *pgxpool.Pool
	stores                *appcatalog.Stores
	cat                   *appcatalog.Catalog
	plans                 *engine.Engine
	users                 map[string]string
	denied                *string
	probes                *atomic.Int64
	alpha, beta, local    *host.Host
	free, paid, betaFree  *policy.Policy
	work, other, bobsAcct *account.Account
	workBinding           *policybinding.PolicyBinding
	teamOne, teamTwo      *team.Team
	proj                  *project.Project
}

func newHostKeyFormWorld(t *testing.T, opts ...func(*Deps)) *hostKeyFormWorld {
	t.Helper()
	ctx := t.Context()
	w := &hostKeyFormWorld{denied: new(string), probes: new(atomic.Int64), users: map[string]string{}}
	w.pool = storagetest.Pool(t)
	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: w.pool, MasterKey: masterKey})
	must(t, "bootstrap", err)
	w.cat, w.stores = cat, stores
	must(t, "seed roles", role.SeedBuiltins(ctx, stores.Role, nil, nil))
	roles, err := stores.Role.List(ctx)
	must(t, "roles", err)
	roleID := map[string]string{}
	for _, r := range roles {
		roleID[r.Meta.Name] = r.Meta.ID
	}
	users := user.NewStore(w.pool)
	for _, name := range []string{"alice", "bob", "carol"} {
		w.users[name] = ids.New()
		must(t, "user", users.Upsert(ctx, &user.User{ID: w.users[name], Username: name}))
	}

	answers := map[string]struct {
		status int
		org    string
	}{
		keyOrgA: {200, "org-a"}, keyOrgA2: {200, "org-a"}, keyOrgB: {200, "org-b"}, keyOrgC: {200, "org-c"},
		keyBare: {200, ""}, keyBusy: {503, ""}, keyRevoked: {401, ""},
	}
	up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.probes.Add(1)
		a, ok := answers[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok || r.URL.Path != "/v1/models" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		if a.org != "" {
			rw.Header().Set("X-Org-Id", a.org)
			rw.Header().Set("X-Org-Name", strings.ToUpper(a.org))
		}
		rw.WriteHeader(a.status)
	}))
	t.Cleanup(up.Close)

	system := meta.Owner{Kind: meta.OwnerSystem}
	w.alpha = &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "alpha", Owner: system}, Spec: host.Spec{BaseURL: up.URL}}
	w.beta = &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "beta", Owner: system}, Spec: host.Spec{BaseURL: "https://beta.example.com"}}
	w.local = &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "local", Owner: system}, Spec: host.Spec{BaseURL: "http://local.example.com", NoAuth: true}}
	for _, h := range []*host.Host{w.alpha, w.beta, w.local} {
		must(t, "host "+h.Meta.Name, stores.Host.Upsert(ctx, h))
	}
	prov := &provider.Provider{Meta: meta.Metadata{ID: ids.New(), Name: "alpha-provider", Owner: system}}
	must(t, "provider", stores.Provider.Upsert(ctx, prov))
	m := &model.Model{Meta: meta.Metadata{ID: ids.New(), Name: "alpha-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "alpha-model"}}, Pointer: "alpha-model"}}
	must(t, "model", stores.Model.Upsert(ctx, m))
	must(t, "binding", stores.Binding.Upsert(ctx, &binding.Binding{Meta: meta.Metadata{ID: ids.New(), Name: "alpha-model-at-alpha", Owner: system}, Spec: binding.Spec{ModelID: m.Meta.ID, HostID: w.alpha.Meta.ID, Adapter: adapters.OpenAI}}))
	tier := func(name string, h *host.Host) *policy.Policy {
		p := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}}}
		must(t, "tier "+name, stores.Policy.Upsert(ctx, p))
		return p
	}
	w.free, w.paid, w.betaFree = tier("alpha-free", w.alpha), tier("alpha-paid", w.alpha), tier("beta-free", w.beta)

	w.teamOne = &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "one", Owner: system}}
	w.teamTwo = &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "two", Owner: system}}
	for _, tm := range []*team.Team{w.teamOne, w.teamTwo} {
		must(t, "team", stores.Team.Upsert(ctx, tm))
	}
	w.proj = &project.Project{Meta: meta.Metadata{ID: ids.New(), Name: "two-app"}, Spec: project.Spec{TeamID: w.teamTwo.Meta.ID}}
	w.proj.StampOwner()
	must(t, "project", stores.Project.Upsert(ctx, w.proj))
	two := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}
	yes := true
	for _, g := range []struct{ who, role string }{{"bob", "team-admin"}, {"alice", "viewer"}} {
		rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: g.who + "-" + g.role, Owner: two}}
		rb.Spec.RoleID, rb.Spec.Scope, rb.Spec.Enabled = roleID[g.role], two, &yes
		rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: w.users[g.who]}}
		must(t, "role binding", stores.RoleBinding.Upsert(ctx, rb))
		_, err := stores.Account.EnsureManaged(ctx, account.Member(w.users[g.who], g.who, two, "two", ""))
		must(t, "member account", err)
	}
	for _, a := range []*account.Account{account.ForTeam(w.teamTwo.Meta.ID, "two", ""), account.ForProject(w.proj.Meta.ID, "two-app", "")} {
		_, err := stores.Account.EnsureManaged(ctx, a)
		must(t, "caller account", err)
	}

	alice := meta.Owner{Kind: meta.OwnerUser, ID: w.users["alice"]}
	w.work = &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "alice-work", Owner: alice}, Spec: account.Spec{HostID: w.alpha.Meta.ID, OrgID: "org-a"}}
	w.other = &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "alice-other", Owner: alice}, Spec: account.Spec{HostID: w.alpha.Meta.ID, OrgID: "org-b"}}
	w.bobsAcct = &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "bob-alpha", Owner: meta.Owner{Kind: meta.OwnerUser, ID: w.users["bob"]}}, Spec: account.Spec{HostID: w.alpha.Meta.ID}}
	for _, a := range []*account.Account{w.work, w.other, w.bobsAcct} {
		must(t, "account "+a.Meta.Name, stores.Account.Upsert(ctx, a))
	}
	w.workBinding = &policybinding.PolicyBinding{Meta: meta.Metadata{ID: ids.New(), Name: "alpha-paid-alice-work", Owner: alice}, Spec: policybinding.Spec{PolicyID: w.paid.Meta.ID, AccountIDs: []string{w.work.Meta.ID}}}
	must(t, "work binding", stores.PolicyBinding.Upsert(ctx, w.workBinding))
	must(t, "reload", cat.Reload(ctx))

	deps := mountDeps(t)
	deps.Stores, deps.Catalog, deps.Users = stores, cat, users
	deps.Authz = audit.Authorizer{Inner: denyAction{Inner: authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}, action: w.denied}, Snap: cat.Current}
	deps.PlanView = func() *engine.View {
		if w.plans == nil {
			return nil
		}
		return w.plans.View()
	}
	deps.PlanCheck = plan.NewCheck(true, time.Minute)
	deps.PseudoKinds = []string{"host-keys"}
	deps.Adapters = adapter.NewRegistry((&adapter.Spec{
		Name: adapters.OpenAI,
		Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Org:  &adapter.OrgProbe{Path: "/v1/models", OrgHeader: "X-Org-Id", NameHeader: "X-Org-Name"},
	}).Build())
	for _, opt := range opts {
		opt(&deps)
	}
	actors := map[string]*actor.Actor{"root": {AdminToken: true, Username: "admin-token"}}
	for name, id := range w.users {
		actors[name] = &actor.Actor{UserID: id, Username: name, SessionID: "s-" + name, Subjects: appcatalog.UserSubjects(id, nil, nil)}
	}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-User", actors))
	Mount(r, deps)
	w.r = r
	w.loadPlans(t)
	return w
}

// loadPlans loads the plans from Postgres; they hear no later write.
func (w *hostKeyFormWorld) loadPlans(t *testing.T) {
	t.Helper()
	cells := cell.NewRegistry(cell.Options{})
	w.plans = engine.New(engine.Options{Cells: cells})
	s := w.stores
	plan.Register(w.plans, cells, plan.Sources{
		Providers: s.Provider, Hosts: s.Host, Models: s.Model, Bindings: s.Binding, Pricings: s.Pricing,
		Policies: s.Policy, PolicyBindings: s.PolicyBinding, RateLimits: s.RateLimit, Budgets: s.Budget, Pools: s.Pool,
		Accounts: s.Account, Credentials: s.HostKey, Teams: s.Team, Projects: s.Project, ServiceAccounts: s.ServiceAccount,
		Users: s.Users, Keys: s.Key, Roles: s.Role, RoleBindings: s.RoleBinding, Groups: s.Group, RBAC: true,
	})
	must(t, "load plans", w.plans.Load(t.Context()))
	t.Cleanup(func() { _ = w.plans.Close() })
}

func (w *hostKeyFormWorld) post(t *testing.T, who string, body map[string]any, dryRun bool) (*httptest.ResponseRecorder, PseudoHostKeyCreated) {
	t.Helper()
	path := "/pseudo-host-keys"
	if dryRun {
		path += "?dryRun=true"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(mustJSON(t, body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", who)
	rec := httptest.NewRecorder()
	w.r.ServeHTTP(rec, req)
	var out PseudoHostKeyCreated
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (w *hostKeyFormWorld) owner(who string) map[string]string {
	return map[string]string{"kind": "user", "id": w.users[who]}
}

func (w *hostKeyFormWorld) body(who string, spec map[string]any) map[string]any {
	spec["hostId"] = w.alpha.Meta.ID
	return map[string]any{"metadata": map[string]any{"displayName": "Work key", "owner": w.owner(who)}, "spec": spec}
}

// counts counts the rows a host key form makes, and the stored secret values.
func (w *hostKeyFormWorld) counts(t *testing.T) map[string]int {
	t.Helper()
	ctx := t.Context()
	accs, err := w.stores.Account.List(ctx)
	must(t, "accounts", err)
	pbs, err := w.stores.PolicyBinding.List(ctx)
	must(t, "policy bindings", err)
	keys, err := w.stores.HostKey.List(ctx)
	must(t, "host keys", err)
	values, err := gen.New(w.pool).ListSecretValues(ctx, gen.ListSecretValuesParams{After: "", MaxRows: 1000})
	must(t, "secret values", err)
	return map[string]int{"account": len(accs), "policy-binding": len(pbs), "host-key": len(keys), "secret-value": len(values)}
}

func createdNames(c PseudoHostKeyCreated) []string {
	var out []string
	for _, r := range c.Created {
		out = append(out, r.Kind+"/"+r.Name)
	}
	return out
}

// A new account takes the organization the host names and is bound to the tier; the key's value is sealed and resolves; the account is shared with team two, and alice, who owns it, is answered as reaching it. Each row is audited as its own create.
func TestPseudoHostKey_NewAccount(t *testing.T) {
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	w := newHostKeyFormWorld(t, func(d *Deps) { d.Audit = em })
	ctx := t.Context()
	toTwo := map[string]string{"kind": "team", "id": w.teamTwo.Meta.ID}
	toAlice := map[string]string{"kind": "user", "id": w.users["alice"]}
	body := w.body("alice", map[string]any{"value": keyOrgC, "account": map[string]any{"displayName": "Work C"}, "tierPolicyId": w.paid.Meta.ID, "pricingStrategy": "api", "shareWith": []any{toTwo, toAlice}})

	rec, out := w.post(t, "alice", body, false)
	wantStatus(t, rec, http.StatusCreated)
	em.Close()
	t.Logf("request: %s\nresponse %d: %s", mustJSON(t, body), rec.Code, rec.Body)
	if diff := cmp.Diff([]string{"account/work-c", "policy-binding/alpha-paid-work-c", "host-key/work-key", "account/work-c-for-two"}, createdNames(out)); diff != "" {
		t.Fatalf("created (-want +got):\n%s", diff)
	}
	if out.Detection != "detected" || out.Detected == nil || *out.Detected != (DetectedOrganization{OrgID: "org-c", OrgName: "ORG-C"}) {
		t.Errorf("detection %q, detected %+v", out.Detection, out.Detected)
	}
	acc, err := w.stores.Account.Get(ctx, out.Account.ID)
	must(t, "account", err)
	alice := meta.Owner{Kind: meta.OwnerUser, ID: w.users["alice"]}
	if acc == nil || acc.Spec.OrgID != "org-c" || acc.Meta.Owner != alice || acc.Spec.HostID != w.alpha.Meta.ID {
		t.Fatalf("account = %+v", acc)
	}
	if held, err := w.stores.PolicyBinding.Holding(ctx, acc.Meta.ID); err != nil || held == nil || held.Spec.PolicyID != w.paid.Meta.ID {
		t.Fatalf("account held by %+v (%v), want the paid tier", held, err)
	}
	k, err := w.stores.HostKey.Get(ctx, out.HostKey.ID)
	must(t, "host key", err)
	if k == nil || k.Spec.ValueFrom.Kind != hostkey.ValueKindStored || k.Resolved != keyOrgC || k.Spec.AccountID != acc.Meta.ID || k.Meta.Owner != alice {
		t.Fatalf("host key = %+v", k)
	}
	share, err := w.stores.Account.Get(ctx, out.Shares[0].ID)
	must(t, "share", err)
	wantShares := []PseudoHostKeyShare{
		{Receiver: account.Receiver{Kind: account.ReceiverTeam, ID: w.teamTwo.Meta.ID}, ID: share.Meta.ID, Name: "work-c-for-two"},
		{Receiver: account.Receiver{Kind: account.ReceiverUser, ID: w.users["alice"]}, ID: acc.Meta.ID, Name: acc.Meta.Name, Existing: true},
	}
	if diff := cmp.Diff(wantShares, out.Shares); diff != "" || share.Spec.SourceAccountID != acc.Meta.ID {
		t.Errorf("shares (-want +got):\n%s; share %+v", diff, share)
	}
	var actions []string
	for _, ev := range sink.all() {
		if ev.Outcome.Status == audit.StatusAllowed && strings.HasSuffix(ev.Action, ".create") {
			actions = append(actions, ev.Action+" "+ev.Resource.ID)
		}
	}
	want := []string{"accounts.create " + acc.Meta.ID, "policy-bindings.create " + out.Created[1].ID, "host-keys.create " + k.Meta.ID, "accounts.create " + share.Meta.ID}
	if diff := cmp.Diff(want, actions); diff != "" {
		t.Errorf("audit (-want +got):\n%s", diff)
	}
}

// An existing account is rebound to the tier through PUT /accounts/{ref}/policy's write: the binding that held it alone goes. A second key in it with the share already made answers that share as existing and makes nothing but the key.
func TestPseudoHostKey_ExistingAccount(t *testing.T) {
	w := newHostKeyFormWorld(t)
	ctx := t.Context()
	toTwo := map[string]string{"kind": "team", "id": w.teamTwo.Meta.ID}

	rec, out := w.post(t, "alice", w.body("alice", map[string]any{"value": keyOrgA, "accountId": w.work.Meta.ID, "tierPolicyId": w.free.Meta.ID, "shareWith": []any{toTwo}}), false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff([]string{"policy-binding/alpha-free-alice-work", "host-key/work-key", "account/alice-work-for-two"}, createdNames(out)); diff != "" {
		t.Fatalf("created (-want +got):\n%s", diff)
	}
	if out.Account != (RowRef{ID: w.work.Meta.ID, Name: w.work.Meta.Name}) || out.Detection != "detected" {
		t.Errorf("account %+v, detection %q", out.Account, out.Detection)
	}
	held, err := w.stores.PolicyBinding.Holding(ctx, w.work.Meta.ID)
	must(t, "holding", err)
	if held == nil || held.Meta.ID != out.Created[0].ID || held.Spec.PolicyID != w.free.Meta.ID {
		t.Fatalf("work held by %+v, want the free tier binding answered", held)
	}
	if old, err := w.stores.PolicyBinding.Get(ctx, w.workBinding.Meta.ID); err != nil || old != nil {
		t.Fatalf("the paid binding that held only work is %+v (%v), want it deleted", old, err)
	}

	before := w.counts(t)
	rec, again := w.post(t, "alice", w.body("alice", map[string]any{"value": keyOrgA2, "accountId": w.work.Meta.ID, "tierPolicyId": w.free.Meta.ID, "shareWith": []any{toTwo}}), false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff([]string{"host-key/work-key-2"}, createdNames(again)); diff != "" {
		t.Errorf("created (-want +got):\n%s", diff)
	}
	wantShare := []PseudoHostKeyShare{{Receiver: account.Receiver{Kind: account.ReceiverTeam, ID: w.teamTwo.Meta.ID}, ID: out.Created[2].ID, Name: "alice-work-for-two", Existing: true}}
	if diff := cmp.Diff(wantShare, again.Shares); diff != "" {
		t.Errorf("shares (-want +got):\n%s", diff)
	}
	after := w.counts(t)
	if after["host-key"]-before["host-key"] != 1 || after["account"] != before["account"] || after["policy-binding"] != before["policy-binding"] || after["secret-value"]-before["secret-value"] != 1 {
		t.Errorf("rows before %v after %v, want one more host key and its value only", before, after)
	}
}

// valueFrom is not checked against the host, and a host that takes no key gets an account alone; neither probes the host. A value for a host that takes none is refused before the host is asked.
func TestPseudoHostKey_ValueFromAndKeyless(t *testing.T) {
	w := newHostKeyFormWorld(t)
	ctx := t.Context()
	t.Setenv("HKFORM_ENV_KEY", keyOrgB)

	rec, out := w.post(t, "root", w.body("alice", map[string]any{"valueFrom": map[string]string{"kind": "env", "env": "HKFORM_ENV_KEY"}, "accountId": w.other.Meta.ID}), false)
	wantStatus(t, rec, http.StatusCreated)
	k, err := w.stores.HostKey.Get(ctx, out.HostKey.ID)
	must(t, "host key", err)
	if k == nil || k.Spec.ValueFrom != (hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "HKFORM_ENV_KEY"}) || k.Spec.AccountID != w.other.Meta.ID || out.Detection != "" || out.Detected != nil {
		t.Fatalf("host key %+v, detection %q %+v", k, out.Detection, out.Detected)
	}

	keyless := func(spec map[string]any) map[string]any {
		b := w.body("alice", spec)
		b["spec"].(map[string]any)["hostId"] = w.local.Meta.ID
		return b
	}
	rec, out = w.post(t, "alice", keyless(map[string]any{"account": map[string]any{"displayName": "Local"}}), false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff([]string{"account/local"}, createdNames(out)); diff != "" || out.HostKey != nil || out.Detection != "" {
		t.Errorf("created (-want +got):\n%s; host key %+v, detection %q", diff, out.HostKey, out.Detection)
	}
	rec, _ = w.post(t, "alice", keyless(map[string]any{"value": keyOrgA, "account": map[string]any{}}), false)
	if r := refusalOf(t, rec); rec.Code != http.StatusBadRequest || r.Pointer != "/spec/value" {
		t.Errorf("value for a keyless host: %d %s", rec.Code, rec.Body)
	}
	if n := w.probes.Load(); n != 0 {
		t.Errorf("the host was asked %d times, want none", n)
	}
}

// A host that names no organization, or does not answer, refuses nothing: the answer says which.
func TestPseudoHostKey_DetectionDoesNotRefuse(t *testing.T) {
	w := newHostKeyFormWorld(t)
	for value, want := range map[string]string{keyBare: "unsupported", keyBusy: "unreachable"} {
		rec, out := w.post(t, "alice", w.body("alice", map[string]any{"value": value, "account": map[string]any{"displayName": want}}), false)
		wantStatus(t, rec, http.StatusCreated)
		acc, err := w.stores.Account.Get(t.Context(), out.Account.ID)
		must(t, "account", err)
		if out.Detection != want || out.Detected != nil || acc.Spec.OrgID != "" {
			t.Errorf("%s: detection %q, detected %+v, account org %q", want, out.Detection, out.Detected, acc.Spec.OrgID)
		}
	}
}

// A dry run asks the host once, answers what would be made with ids, and writes no row and no secret value.
func TestPseudoHostKey_DryRun(t *testing.T) {
	w := newHostKeyFormWorld(t)
	before := w.counts(t)
	body := w.body("alice", map[string]any{"value": keyOrgC, "account": map[string]any{"displayName": "Work C"}, "tierPolicyId": w.paid.Meta.ID, "shareWith": []any{map[string]string{"kind": "team", "id": w.teamTwo.Meta.ID}}})

	rec, dry := w.post(t, "alice", body, true)
	wantStatus(t, rec, http.StatusOK)
	t.Logf("request (dryRun=true): %s\nresponse %d: %s", mustJSON(t, body), rec.Code, rec.Body)
	if dry.HostKey == nil || dry.HostKey.ID == "" || dry.Account.ID == "" || len(dry.Created) != 4 || dry.Detection != "detected" {
		t.Fatalf("dry run = %+v", dry)
	}
	if diff := cmp.Diff(before, w.counts(t)); diff != "" {
		t.Fatalf("a dry run wrote (-before +after):\n%s", diff)
	}
	if n := w.probes.Load(); n != 1 {
		t.Errorf("the host was asked %d times, want once", n)
	}

	rec, made := w.post(t, "alice", body, false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff(createdNames(dry), createdNames(made)); diff != "" {
		t.Errorf("the write made other rows than the dry run answered (-dry +made):\n%s", diff)
	}
}

// Each refusal names the part of the body at fault and leaves no row and no secret value, the ones refused after the value was sealed included.
func TestPseudoHostKey_Refusals(t *testing.T) {
	w := newHostKeyFormWorld(t)
	newAcct := map[string]any{"displayName": "Work"}
	for _, tc := range []struct {
		name, who, deny, code, pointer string
		spec                           map[string]any
		status                         int
		holder                         *account.Account
	}{
		{name: "the provider refuses the key", who: "alice", spec: map[string]any{"value": keyRevoked, "account": newAcct}, status: 422, code: codeProviderRefusedKey, pointer: "/spec/value"},
		{name: "another host's tier", who: "alice", spec: map[string]any{"value": keyOrgC, "account": newAcct, "tierPolicyId": w.betaFree.Meta.ID}, status: 400, pointer: "/spec/tierPolicyId"},
		{name: "an organization another account of the owner holds", who: "alice", spec: map[string]any{"value": keyOrgB, "accountId": w.work.Meta.ID}, status: 409, code: codeOrganizationTaken, pointer: "/spec/accountId", holder: w.other},
		{name: "a new account for an organization the owner holds", who: "alice", spec: map[string]any{"value": keyOrgA, "account": newAcct}, status: 409, code: codeOrganizationTaken, pointer: "/spec/account", holder: w.work},
		{name: "an account by slug", who: "alice", spec: map[string]any{"value": keyOrgA, "accountId": "alice-work"}, status: 400, pointer: "/spec/accountId"},
		{name: "an account the caller may not see", who: "alice", spec: map[string]any{"value": keyOrgC, "accountId": w.bobsAcct.Meta.ID}, status: 404, pointer: "/spec/accountId"},
		{name: "another user as owner", who: "carol", spec: map[string]any{"value": keyOrgC, "account": newAcct}, status: 400, pointer: "/metadata/owner"},
		{name: "the account's grant", who: "alice", deny: "accounts.create", spec: map[string]any{"value": keyOrgC, "account": newAcct}, status: 403, pointer: "/spec/account"},
		{name: "the tier binding's grant", who: "alice", deny: "policy-bindings.create", spec: map[string]any{"value": keyOrgC, "account": newAcct, "tierPolicyId": w.paid.Meta.ID}, status: 403, pointer: "/spec/tierPolicyId"},
		{name: "the host key's grant, before its value is sealed", who: "alice", deny: "host-keys.create", spec: map[string]any{"value": keyOrgC, "account": newAcct}, status: 403, pointer: "/metadata/owner"},
		{name: "an env value from a non-admin", who: "alice", spec: map[string]any{"valueFrom": map[string]string{"kind": "env", "env": "HKFORM_ENV_KEY"}, "account": newAcct}, status: 403, pointer: "/metadata"},
		{name: "a share with a receiver the caller may not see", who: "alice", spec: map[string]any{"value": keyOrgC, "account": newAcct, "shareWith": []any{map[string]string{"kind": "team", "id": w.teamOne.Meta.ID}}}, status: 404, pointer: "/spec/shareWith/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*w.denied = tc.deny
			defer func() { *w.denied = "" }()
			before := w.counts(t)
			body := w.body("alice", tc.spec)
			rec, _ := w.post(t, tc.who, body, false)
			if tc.status == 422 || tc.status == 409 && tc.pointer == "/spec/accountId" {
				t.Logf("request: %s\nresponse %d: %s", mustJSON(t, body), rec.Code, rec.Body)
			}
			if rec.Code != tc.status {
				t.Fatalf("status %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
			var r pseudoOrganizationRefusal
			must(t, "decode "+rec.Body.String(), json.Unmarshal(rec.Body.Bytes(), &r))
			if r.Pointer != tc.pointer || (tc.code != "" && r.Err.Code != tc.code) {
				t.Errorf("pointer %q code %q, want %q %q", r.Pointer, r.Err.Code, tc.pointer, tc.code)
			}
			if tc.holder != nil && (r.Account == nil || *r.Account != (RowRef{ID: tc.holder.Meta.ID, Name: tc.holder.Meta.Name})) {
				t.Errorf("account %+v, want %s", r.Account, tc.holder.Meta.Name)
			}
			if diff := cmp.Diff(before, w.counts(t)); diff != "" {
				t.Errorf("a refused request wrote (-before +after):\n%s", diff)
			}
		})
	}
}

// A write is audited as each row's create; a request refused before its write or during it as one row naming the request; a dry run that asked the host as one dry-run row on the host, whatever it answered; a dry run refused before the host was asked as nothing.
func TestPseudoHostKey_Audit(t *testing.T) {
	type entry struct {
		Action, Kind, Status string
		Code                 int
	}
	refused := func(status string, code int) []entry {
		return []entry{{"pseudo-host-keys.create", "pseudo-host-key", status, code}}
	}
	probed := func(status string, code int) []entry {
		return []entry{{"pseudo-host-keys.dry-run", "host", status, code}}
	}
	for _, tc := range []struct {
		name, deny, value string
		dry               bool
		want              []entry
	}{
		{name: "made", value: keyOrgC, want: []entry{{"accounts.create", "account", audit.StatusAllowed, 201}, {"host-keys.create", "host-key", audit.StatusAllowed, 201}}},
		{name: "refused by the provider", value: keyRevoked, want: refused(audit.StatusError, 422)},
		{name: "a row refused", value: keyOrgC, deny: "host-keys.create", want: refused(audit.StatusDenied, 403)},
		{name: "a dry run", value: keyOrgC, dry: true, want: probed(audit.StatusAllowed, 200)},
		{name: "a dry run refused by the provider", value: keyRevoked, dry: true, want: probed(audit.StatusError, 422)},
		{name: "a dry run refused before the host was asked", value: keyOrgC, deny: "host-keys.create", dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &auditSink{}
			em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
			w := newHostKeyFormWorld(t, func(d *Deps) { d.Audit = em })
			*w.denied = tc.deny
			w.post(t, "alice", w.body("alice", map[string]any{"value": tc.value, "account": map[string]any{"displayName": "Work"}}), tc.dry)
			em.Close()
			var got []entry
			for _, ev := range sink.all() {
				if strings.HasSuffix(ev.Request.Path, "/pseudo-host-keys") {
					got = append(got, entry{ev.Action, ev.Resource.Kind, ev.Outcome.Status, ev.Outcome.Code})
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("audit (-want +got):\n%s", diff)
			}
		})
	}
}

// lockedBuffer is a log sink the handlers may write to from their own goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The value appears in no answer, log line or audit row: of a write, a dry run, a request the provider refused, one refused after the value was sealed, and one the server failed.
func TestPseudoHostKey_ValueNeverShown(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.Default())
	w := newHostKeyFormWorld(t, func(d *Deps) { d.Audit = em })

	var answers []string
	send := func(who, deny, value string, dry bool, status int) {
		t.Helper()
		*w.denied = deny
		defer func() { *w.denied = "" }()
		rec, _ := w.post(t, who, w.body("alice", map[string]any{"value": value, "account": map[string]any{"displayName": "Work"}}), dry)
		if rec.Code != status {
			t.Fatalf("status %d %s, want %d", rec.Code, rec.Body, status)
		}
		answers = append(answers, rec.Body.String())
	}
	send("alice", "", keyOrgC, false, http.StatusCreated)
	send("alice", "", keyBare, true, http.StatusOK)
	send("alice", "", keyRevoked, false, http.StatusUnprocessableEntity)
	send("alice", "host-keys.create", keyBusy, false, http.StatusForbidden)
	w.pool.Close()
	send("alice", "", keyOrgB, false, http.StatusInternalServerError)
	em.Close()

	events, err := json.Marshal(sink.all())
	must(t, "audit rows", err)
	seen := strings.Join(answers, "\n") + "\n" + logs.String() + "\n" + string(events)
	for _, v := range []string{keyOrgC, keyBare, keyRevoked, keyBusy, keyOrgB} {
		if strings.Contains(seen, v) {
			t.Errorf("%s appears in an answer, a log line or an audit row", v)
		}
	}
	if !strings.Contains(logs.String(), "pseudo write failed") {
		t.Errorf("the server failure was not logged; the check above saw no log line:\n%s", logs.String())
	}
}

var _ = context.Background

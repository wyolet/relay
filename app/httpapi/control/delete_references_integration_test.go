//go:build integration

// delete_references_integration_test.go deletes a row of every kind through
// the real handler against Postgres: a referenced row is refused with its
// blockers and left in place, an unreferenced one is deleted.
// Run with: make test-integration.
package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
)

type wantGroup struct {
	kind, field string
	detachable  bool
	count       int
}

// wantBlockers asserts a 409 whose blocker groups are exactly want, in order.
func wantBlockers(t *testing.T, w *httptest.ResponseRecorder, want ...wantGroup) inUseResponse {
	t.Helper()
	body := decodeInUse(t, w)
	got := make([]wantGroup, len(body.Blockers))
	for i, g := range body.Blockers {
		got[i] = wantGroup{g.Kind, g.Field, g.Detachable, g.Count}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("blockers = %+v, want %+v", got, want)
	}
	return body
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status = %d, want %d: %s", w.Code, code, w.Body.String())
	}
}

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// allowCatalogDeletes turns on the governance sections that otherwise refuse
// every delete of a catalog-managed kind, and loads them into the catalog.
func (f policyDeleteFixture) allowCatalogDeletes(t *testing.T, ctx context.Context, sections ...string) {
	t.Helper()
	for _, s := range sections {
		_, err := f.stores.Settings.Upsert(ctx, s, json.RawMessage(`{"allowEdit":true,"allowDelete":true}`))
		must(t, "upsert "+s, err)
	}
	_, err := f.cat.Hydrate(ctx, f.stores, appcatalog.BootstrapOptions{Pool: f.pool})
	must(t, "hydrate", err)
}

func (f policyDeleteFixture) provider(t *testing.T, ctx context.Context, name string) *provider.Provider {
	t.Helper()
	p := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	must(t, "upsert provider", f.stores.Provider.Upsert(ctx, p))
	return p
}

func (f policyDeleteFixture) model(t *testing.T, ctx context.Context, name, providerID string) *model.Model {
	t.Helper()
	m := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: providerID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: name}}, Pointer: name},
	}
	must(t, "upsert model", f.stores.Model.Upsert(ctx, m))
	return m
}

func (f policyDeleteFixture) pricing(t *testing.T, ctx context.Context, name string, modelIDs ...string) *pricing.Pricing {
	t.Helper()
	p := &pricing.Pricing{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerHost, ID: f.host.Meta.ID}},
		Spec: pricing.Spec{
			Currency: "USD", TargetModelIDs: modelIDs,
			Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1}},
		},
	}
	must(t, "upsert pricing", f.stores.Pricing.Upsert(ctx, p))
	return p
}

func (f policyDeleteFixture) hostBinding(t *testing.T, ctx context.Context, name, modelID, pricingID string) *binding.Binding {
	t.Helper()
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modelID, HostID: f.host.Meta.ID, Adapter: adapters.OpenAI, PricingID: pricingID},
	}
	must(t, "upsert host binding", f.stores.Binding.Upsert(ctx, b))
	return b
}

func (f policyDeleteFixture) rateLimit(t *testing.T, ctx context.Context, name string, owner meta.Owner) *ratelimit.RateLimit {
	t.Helper()
	r := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 10,
			Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyFixedWindow,
		}}},
	}
	must(t, "upsert rate limit", f.stores.RateLimit.Upsert(ctx, r))
	return r
}

func (f policyDeleteFixture) hostKey(t *testing.T, ctx context.Context, name, policyID string) *hostkey.HostKey {
	t.Helper()
	t.Setenv("DEL_HOSTKEY_VALUE", "sk-test-value")
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: f.host.Meta.ID, PolicyID: policyID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "DEL_HOSTKEY_VALUE"}},
	}
	must(t, "upsert host key", f.stores.HostKey.Upsert(ctx, k))
	return k
}

func (f policyDeleteFixture) customRole(t *testing.T, ctx context.Context, name string) *role.Role {
	t.Helper()
	r := &role.Role{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}},
	}
	must(t, "upsert role", f.stores.Role.Upsert(ctx, r))
	return r
}

func (f policyDeleteFixture) roleBinding(t *testing.T, ctx context.Context, name, roleID string, scope meta.Owner, subjects ...rolebinding.Subject) *rolebinding.RoleBinding {
	t.Helper()
	b := &rolebinding.RoleBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name},
		Spec: rolebinding.Spec{RoleID: roleID, Scope: scope, Subjects: subjects},
	}
	b.StampOwner()
	must(t, "upsert role binding", f.stores.RoleBinding.Upsert(ctx, b))
	return b
}

func (f policyDeleteFixture) policyBinding(t *testing.T, ctx context.Context, name, policyID string, subjects ...rolebinding.Subject) *policybinding.PolicyBinding {
	t.Helper()
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name},
		Spec: policybinding.Spec{ProjectID: f.proj.Meta.ID, PolicyID: policyID, Subjects: subjects},
	}
	b.StampOwner()
	must(t, "upsert policy binding", f.stores.PolicyBinding.Upsert(ctx, b))
	return b
}

func (f policyDeleteFixture) projectOwner() meta.Owner {
	return meta.Owner{Kind: meta.OwnerProject, ID: f.proj.Meta.ID}
}

func TestDeleteProvider_OwnedModelsAndRateLimitsBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	prov := f.provider(t, ctx, "vendor")
	m := f.model(t, ctx, "vendor-model", prov.Meta.ID)
	f.rateLimit(t, ctx, "vendor-tier", meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID})

	wantBlockers(t, f.deleteRow(t, "providers", prov.Meta.ID, false),
		wantGroup{"model", "metadata.owner.id", false, 1},
		wantGroup{"rate-limit", "metadata.owner.id", false, 1},
	)
	if got, err := f.stores.Model.Get(ctx, m.Meta.ID); err != nil || got == nil {
		t.Fatalf("model after refused delete: %v", err)
	}
}

// An unreferenced system row is the admin's to delete; nobody else's.
func TestDeleteProvider_UnreferencedSystemRowIsAdminOnly(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	prov := f.provider(t, ctx, "unused-vendor")

	wantStatus(t, f.deleteRow(t, "providers", prov.Meta.ID, true), http.StatusForbidden)
	wantStatus(t, f.deleteRow(t, "providers", prov.Meta.ID, false), http.StatusNoContent)
	if got, _ := f.stores.Provider.Get(ctx, prov.Meta.ID); got != nil {
		t.Fatal("the provider is still stored")
	}
}

func TestDeleteHost_EveryRowOnTheHostBlocks(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	m := f.model(t, ctx, "host-model", f.provider(t, ctx, "host-vendor").Meta.ID)
	tier := f.policy(t, ctx, "host-tier", meta.Owner{Kind: meta.OwnerHost, ID: f.host.Meta.ID})
	f.hostKey(t, ctx, "host-key", tier.Meta.ID)
	f.hostBinding(t, ctx, "host-binding", m.Meta.ID, "")
	f.pricing(t, ctx, "host-pricing", m.Meta.ID)
	f.rateLimit(t, ctx, "host-limit", meta.Owner{Kind: meta.OwnerHost, ID: f.host.Meta.ID})

	wantBlockers(t, f.deleteRow(t, "hosts", f.host.Meta.ID, false),
		wantGroup{"host-binding", "spec.hostId", false, 1},
		wantGroup{"host-key", "spec.hostId", false, 1},
		wantGroup{"policy", "metadata.owner.id", false, 1},
		wantGroup{"pricing", "metadata.owner.id", false, 1},
		wantGroup{"rate-limit", "metadata.owner.id", false, 1},
	)

	spare := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "spare-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://spare.example.com"},
	}
	must(t, "upsert host", f.stores.Host.Upsert(ctx, spare))
	wantStatus(t, f.deleteRow(t, "hosts", spare.Meta.ID, false), http.StatusNoContent)
}

func TestDeleteModel_GrantsPricingsBindingsAndOverlayBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	f.allowCatalogDeletes(t, ctx, "governance:model")
	prov := f.provider(t, ctx, "model-vendor")
	m := f.model(t, ctx, "used-model", prov.Meta.ID)
	other := f.model(t, ctx, "other-model", prov.Meta.ID)

	grant := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "grant", Owner: f.projectOwner()},
		Spec: policy.Spec{ModelIDs: []string{m.Meta.ID}},
	}
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, grant))
	f.pricing(t, ctx, "shared-pricing", m.Meta.ID, other.Meta.ID)
	f.pricing(t, ctx, "own-pricing", m.Meta.ID)
	f.hostBinding(t, ctx, "model-binding", m.Meta.ID, "")
	must(t, "upsert overlay", f.stores.Overlay.Upsert(ctx, &overlay.Overlay{
		Kind: overlay.KindModel, ResourceID: m.Meta.ID, Patch: json.RawMessage(`{"family":"patched"}`),
	}))

	wantBlockers(t, f.deleteRow(t, "models", m.Meta.ID, false),
		wantGroup{"host-binding", "spec.modelId", false, 1},
		wantGroup{"overlay", "resourceId", false, 1},
		wantGroup{"policy", "spec.modelIds", true, 1},
		// A pricing can drop this model only while it prices another.
		wantGroup{"pricing", "spec.targetModels", true, 1},
		wantGroup{"pricing", "spec.targetModels", false, 1},
	)

	unused := f.model(t, ctx, "unused-model", prov.Meta.ID)
	wantStatus(t, f.deleteRow(t, "models", unused.Meta.ID, false), http.StatusNoContent)
}

// A pricing is host-owned and no governance section opens its delete, so the
// references are checked below the handler.
func TestPricingBlockers_BindingsCanLetGo(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	m := f.model(t, ctx, "priced-model", f.provider(t, ctx, "pricing-vendor").Meta.ID)
	used := f.pricing(t, ctx, "used-pricing", m.Meta.ID)
	unused := f.pricing(t, ctx, "unused-pricing", m.Meta.ID)
	b := f.hostBinding(t, ctx, "priced-binding", m.Meta.ID, used.Meta.ID)

	d := Deps{Authz: testRBAC(), Stores: f.stores, Users: f.users}
	ctx = visibleCtx(ctx)
	groups, err := blockers(ctx, d, "pricing", used.Meta.ID)
	must(t, "blockers", err)
	if len(groups) != 1 || groups[0].Kind != "host-binding" || groups[0].Field != "spec.pricingId" ||
		!groups[0].Detachable || len(groups[0].Items) != 1 || groups[0].Items[0].ID != b.Meta.ID {
		t.Fatalf("blockers = %+v, want the binding via spec.pricingId, detachable", groups)
	}
	if groups, err := blockers(ctx, d, "pricing", unused.Meta.ID); err != nil || len(groups) != 0 {
		t.Fatalf("unused pricing blockers = %+v (err %v), want none", groups, err)
	}
}

// The token signing key lives in a stored secret; a host key's delete removes
// the secret kept under its id, so a settings ref to that id blocks it.
func TestDeleteHostKey_PolicyAndSigningKeyRefBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	tier := f.policy(t, ctx, "key-tier", f.projectOwner())
	hk := f.hostKey(t, ctx, "pooled-key", tier.Meta.ID)
	pool := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pool", Owner: f.projectOwner()},
		Spec: policy.Spec{HostKeyIDs: []string{hk.Meta.ID}},
	}
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, pool))
	_, err := f.stores.Settings.Upsert(ctx, "auth:tokens", json.RawMessage(
		`{"enabled":true,"defaultTTL":3600000000000,"maxTTL":86400000000000,"signingKey":{"kind":"stored","id":"`+hk.Meta.ID+`"}}`))
	must(t, "upsert auth:tokens", err)

	wantBlockers(t, f.deleteRow(t, "host-keys", hk.Meta.ID, false),
		wantGroup{"policy", "spec.hostKeyIds", true, 1},
		wantGroup{"settings", "signingKey", false, 1},
	)
	got, err := f.stores.Policy.Get(ctx, pool.Meta.ID)
	if err != nil || got == nil || !slices.Contains(got.Spec.HostKeyIDs, hk.Meta.ID) {
		t.Fatalf("policy after refused delete = %+v (err %v), want it still drawing on the key", got, err)
	}

	unused := f.hostKey(t, ctx, "unused-key", tier.Meta.ID)
	wantStatus(t, f.deleteRow(t, "host-keys", unused.Meta.ID, false), http.StatusNoContent)
}

func TestDeleteRateLimit_PolicyRefsBlockAsDetachable(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	rl := f.rateLimit(t, ctx, "shared-limit", meta.Owner{Kind: meta.OwnerSystem})
	flat := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "flat", Owner: f.projectOwner()},
		Spec: policy.Spec{RateLimitID: rl.Meta.ID},
	}
	perModel := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "per-model", Owner: f.projectOwner()},
		Spec: policy.Spec{RLBindings: []policy.RLBinding{{Models: []string{"some-model"}, RateLimitID: rl.Meta.ID}}},
	}
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, flat))
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, perModel))

	wantBlockers(t, f.deleteRow(t, "rate-limits", rl.Meta.ID, false),
		wantGroup{"policy", "spec.rateLimitId", true, 1},
		wantGroup{"policy", "spec.rlBindings[].rateLimitId", true, 1},
	)
	if got, err := f.stores.Policy.Get(ctx, perModel.Meta.ID); err != nil || got == nil || len(got.Spec.RLBindings) != 1 {
		t.Fatalf("policy after refused delete = %+v (err %v), want its binding kept", got, err)
	}

	unused := f.rateLimit(t, ctx, "unused-limit", meta.Owner{Kind: meta.OwnerSystem})
	wantStatus(t, f.deleteRow(t, "rate-limits", unused.Meta.ID, true), http.StatusForbidden)
	wantStatus(t, f.deleteRow(t, "rate-limits", unused.Meta.ID, false), http.StatusNoContent)
}

// A policy in a project alice can't see still blocks her rate limit, counted
// but never named.
func TestDeleteRateLimit_HiddenReferenceBlocksWithoutItsName(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	rl := f.rateLimit(t, ctx, "alice-limit", meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID})
	own := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-flat", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID}},
		Spec: policy.Spec{RateLimitID: rl.Meta.ID},
	}
	hidden := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "hidden-flat", Owner: f.projectOwner()},
		Spec: policy.Spec{RateLimitID: rl.Meta.ID},
	}
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, own))
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, hidden))

	w := f.deleteRow(t, "rate-limits", rl.Meta.ID, true)
	if strings.Contains(w.Body.String(), hidden.Meta.Name) || strings.Contains(w.Body.String(), hidden.Meta.ID) {
		t.Fatalf("409 body names a row the caller may not see: %s", w.Body.String())
	}
	body := wantBlockers(t, w, wantGroup{"policy", "spec.rateLimitId", true, 2})
	if g := body.Blockers[0]; g.Hidden != 1 || len(g.Items) != 1 || g.Items[0].ID != own.Meta.ID {
		t.Fatalf("group = %+v, want alice's policy listed and one hidden", g)
	}
}

func TestDeleteKey_Unreferenced(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	k := f.projectKey(t, ctx, "key-e", "")
	wantStatus(t, f.deleteRow(t, "keys", k.Meta.ID, false), http.StatusNoContent)
}

func TestDeleteTeam_ProjectsAndScopedBindingsBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "busy-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	must(t, "upsert team", f.stores.Team.Upsert(ctx, tm))
	p := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "busy-project"}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	p.StampOwner()
	must(t, "upsert project", f.stores.Project.Upsert(ctx, p))
	r := f.customRole(t, ctx, "team-reader")
	f.roleBinding(t, ctx, "team-readers", r.Meta.ID, meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID},
		rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: f.alice.ID})

	wantBlockers(t, f.deleteRow(t, "teams", tm.Meta.ID, false),
		wantGroup{"project", "spec.teamId", false, 1},
		wantGroup{"role-binding", "spec.scope", false, 1},
	)
	if got, err := f.stores.Project.Get(ctx, p.Meta.ID); err != nil || got == nil {
		t.Fatalf("project after refused delete: %v", err)
	}

	empty := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "empty-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	must(t, "upsert team", f.stores.Team.Upsert(ctx, empty))
	wantStatus(t, f.deleteRow(t, "teams", empty.Meta.ID, false), http.StatusNoContent)
}

func TestDeleteProject_RowsUnderItBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	f.policy(t, ctx, "project-policy", f.projectOwner())

	wantBlockers(t, f.deleteRow(t, "projects", f.proj.Meta.ID, false),
		wantGroup{"policy", "metadata.owner.id", false, 1},
		wantGroup{"service-account", "spec.projectId", false, 1},
	)

	empty := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "empty-project"}, Spec: project.Spec{TeamID: f.proj.Spec.TeamID}}
	empty.StampOwner()
	must(t, "upsert project", f.stores.Project.Upsert(ctx, empty))
	wantStatus(t, f.deleteRow(t, "projects", empty.Meta.ID, false), http.StatusNoContent)
}

// A binding can let go of a subject only while it names someone else.
func TestDeleteServiceAccount_KeysAndBindingsBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	f.projectKey(t, ctx, "key-d", "")
	r := f.customRole(t, ctx, "indexer-role")
	account := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID}
	f.roleBinding(t, ctx, "shared-grant", r.Meta.ID, f.projectOwner(),
		account, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: f.alice.ID})
	f.policyBinding(t, ctx, "sole-grant", f.policy(t, ctx, "bound", f.projectOwner()).Meta.ID, account)

	wantBlockers(t, f.deleteRow(t, "service-accounts", f.account.Meta.ID, false),
		wantGroup{"key", "spec.principal.id", false, 1},
		wantGroup{"policy-binding", "spec.subjects", false, 1},
		wantGroup{"role-binding", "spec.subjects", true, 1},
	)
}

func TestDeleteGroup_BindingsNamingItBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	must(t, "upsert group", f.stores.Group.Upsert(ctx, g))
	subject := rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: g.Meta.Name}
	f.roleBinding(t, ctx, "group-grant", f.customRole(t, ctx, "group-role").Meta.ID, f.projectOwner(), subject)
	f.policyBinding(t, ctx, "group-policy", f.policy(t, ctx, "group-bound", f.projectOwner()).Meta.ID,
		subject, rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID})

	wantBlockers(t, f.deleteRow(t, "groups", g.Meta.ID, false),
		wantGroup{"policy-binding", "spec.subjects", true, 1},
		wantGroup{"role-binding", "spec.subjects", false, 1},
	)

	unused := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "unused-group", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	must(t, "upsert group", f.stores.Group.Upsert(ctx, unused))
	wantStatus(t, f.deleteRow(t, "groups", unused.Meta.ID, false), http.StatusNoContent)
}

func builtinRole(t *testing.T, ctx context.Context, f policyDeleteFixture, name string) *role.Role {
	t.Helper()
	roles, err := f.stores.Role.List(ctx)
	must(t, "list roles", err)
	for _, r := range roles {
		if r.Meta.Name == name {
			return r
		}
	}
	t.Fatalf("no built-in role %q", name)
	return nil
}

// The admin role is held by its admins, so it can't be deleted while any
// account carries it.
func TestDeleteRole_AdminRoleHeldByAUserBlocks(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	must(t, "seed roles", role.SeedBuiltins(ctx, f.stores.Role, nil, nil))
	root := &user.User{ID: meta.NewID(), Username: "root-" + meta.NewID()[:8], Roles: []string{user.RoleAdmin}}
	must(t, "upsert user", f.users.Upsert(ctx, root))

	body := wantBlockers(t, f.deleteRow(t, "roles", builtinRole(t, ctx, f, "admin").Meta.ID, false),
		wantGroup{"user", "roles", true, 1},
	)
	if items := body.Blockers[0].Items; len(items) != 1 || items[0].ID != root.ID {
		t.Fatalf("items = %+v, want the admin account", items)
	}
}

func TestDeleteRole_BindingsBlock(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	r := f.customRole(t, ctx, "release-manager")
	f.roleBinding(t, ctx, "releases", r.Meta.ID, f.projectOwner(), rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: f.alice.ID})

	wantBlockers(t, f.deleteRow(t, "roles", r.Meta.ID, false),
		wantGroup{"role-binding", "spec.roleId", false, 1},
	)
}

// An unreferenced built-in role is the admin's to delete, and the next boot
// seeds it again.
func TestDeleteRole_UnreferencedBuiltinIsAdminOnlyAndReseeded(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	must(t, "seed roles", role.SeedBuiltins(ctx, f.stores.Role, nil, nil))
	viewer := builtinRole(t, ctx, f, "viewer")

	wantStatus(t, f.deleteRow(t, "roles", viewer.Meta.ID, true), http.StatusForbidden)
	wantStatus(t, f.deleteRow(t, "roles", viewer.Meta.ID, false), http.StatusNoContent)

	must(t, "reseed roles", role.SeedBuiltins(ctx, f.stores.Role, nil, nil))
	builtinRole(t, ctx, f, "viewer")
}

func TestDeleteBindings_Unreferenced(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	m := f.model(t, ctx, "bound-model", f.provider(t, ctx, "binding-vendor").Meta.ID)
	hb := f.hostBinding(t, ctx, "lone-binding", m.Meta.ID, "")
	rb := f.roleBinding(t, ctx, "lone-grant", f.customRole(t, ctx, "lone-role").Meta.ID, f.projectOwner(),
		rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: f.alice.ID})
	pb := f.policyBinding(t, ctx, "lone-policy", f.policy(t, ctx, "lone-bound", f.projectOwner()).Meta.ID,
		rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID})

	wantStatus(t, f.deleteRow(t, "host-bindings", hb.Meta.ID, false), http.StatusNoContent)
	wantStatus(t, f.deleteRow(t, "role-bindings", rb.Meta.ID, false), http.StatusNoContent)
	wantStatus(t, f.deleteRow(t, "policy-bindings", pb.Meta.ID, false), http.StatusNoContent)
}

//go:build integration

// detach_integration_test.go releases references through the real handler
// against Postgres: what can let go is written in one transaction, and every
// other referencing row is reported and left exactly as it was.
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

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
)

type detachResponse struct {
	Detached []blockerGroup `json:"detached"`
	Blockers []blockerGroup `json:"blockers"`
}

// detachRow sends POST detach as the admin token, or as alice when asAlice.
func (f policyDeleteFixture) detachRow(t *testing.T, plural, id string, asAlice bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+plural+"/by-id/"+id+"/detach", nil)
	if asAlice {
		req.Header.Set("X-Test-User", "alice")
	} else {
		req.Header.Set("Authorization", "Bearer test-admin-token")
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func decodeDetach(t *testing.T, w *httptest.ResponseRecorder) detachResponse {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	var body detachResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode detach body: %v: %s", err, w.Body.String())
	}
	return body
}

// wantGroups asserts the groups are exactly want, in order.
func wantGroups(t *testing.T, what string, groups []blockerGroup, want ...wantGroup) {
	t.Helper()
	got := make([]wantGroup, len(groups))
	for i, g := range groups {
		got[i] = wantGroup{g.Kind, g.Field, g.Detachable, g.Count}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %+v, want %+v", what, got, want)
	}
}

// A binding can't let go of its policy: detach releases nothing and the
// binding keeps refusing the delete.
func TestDetachPolicy_KeepsBindings(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	tier := f.policy(t, ctx, "detach-tier", meta.Owner{Kind: meta.OwnerHost, ID: f.host.Meta.ID})
	f.policyBinding(t, ctx, "tier-binding", tier.Meta.ID, f.accountOf(t, ctx, f.account))
	f.allowCatalogDeletes(t, ctx, "governance:policy")

	body := decodeDetach(t, f.detachRow(t, "policies", tier.Meta.ID, false))
	wantGroups(t, "detached", body.Detached)
	wantGroups(t, "blockers", body.Blockers, wantGroup{"policy-binding", "spec.policyId", false, 1})
	wantBlockers(t, f.deletePolicy(t, tier.Meta.ID, false), wantGroup{"policy-binding", "spec.policyId", false, 1})
}

// A list that needs an entry lets go of the model only while it holds another.
func TestDetachModel_PricingKeepsItsLastModel(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	prov := f.provider(t, ctx, "vendor")
	m := f.model(t, ctx, "vendor-model", prov.Meta.ID)
	other := f.model(t, ctx, "vendor-other", prov.Meta.ID)
	shared := f.pricing(t, ctx, "shared-rates", m.Meta.ID, other.Meta.ID)
	only := f.pricing(t, ctx, "only-rates", m.Meta.ID)

	body := decodeDetach(t, f.detachRow(t, "models", m.Meta.ID, false))
	wantGroups(t, "detached", body.Detached, wantGroup{"pricing", "spec.targetModels", true, 1})
	wantGroups(t, "blockers", body.Blockers, wantGroup{"pricing", "spec.targetModels", false, 1})
	got, err := f.stores.Pricing.Get(ctx, shared.Meta.ID)
	must(t, "get shared pricing", err)
	if !slices.Equal(got.Spec.TargetModelIDs, []string{other.Meta.ID}) {
		t.Fatalf("shared pricing models = %v, want only the other model", got.Spec.TargetModelIDs)
	}
	got, err = f.stores.Pricing.Get(ctx, only.Meta.ID)
	must(t, "get only pricing", err)
	if !slices.Equal(got.Spec.TargetModelIDs, []string{m.Meta.ID}) || got.Meta.Dirty {
		t.Fatalf("single-model pricing = %+v, want untouched", got.Spec.TargetModelIDs)
	}
}

// A binding lets go of the service account, or of its account, only while it names another.
func TestDetachServiceAccount_BindingKeepsItsLastSubject(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	second := f.secondAccount(t, ctx)
	acct := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID}
	other := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: second.Meta.ID}
	r := f.customRole(t, ctx, "key-reader")
	shared := f.roleBinding(t, ctx, "shared-binding", r.Meta.ID, f.projectOwner(), acct, other)
	pol := f.policy(t, ctx, "bound-policy", f.projectOwner())
	own := f.accountOf(t, ctx, f.account)
	only := f.policyBinding(t, ctx, "only-binding", pol.Meta.ID, own)

	body := decodeDetach(t, f.detachRow(t, "service-accounts", f.account.Meta.ID, false))
	wantGroups(t, "detached", body.Detached, wantGroup{"role-binding", "spec.subjects", true, 1})
	wantGroups(t, "blockers", body.Blockers, wantGroup{"policy-binding", "spec.accountIds", false, 1})
	rb, err := f.stores.RoleBinding.Get(ctx, shared.Meta.ID)
	must(t, "get role binding", err)
	if len(rb.Spec.Subjects) != 1 || rb.Spec.Subjects[0].ID != second.Meta.ID {
		t.Fatalf("role binding subjects = %+v, want only the second account", rb.Spec.Subjects)
	}
	pb, err := f.stores.PolicyBinding.Get(ctx, only.Meta.ID)
	must(t, "get policy binding", err)
	if !slices.Equal(pb.Spec.AccountIDs, []string{own}) || pb.Meta.Dirty {
		t.Fatalf("policy binding = %+v, want untouched", pb.Spec.AccountIDs)
	}
}

func (f policyDeleteFixture) secondAccount(t *testing.T, ctx context.Context) *serviceaccount.ServiceAccount {
	t.Helper()
	second := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "second-account"},
		Spec: serviceaccount.Spec{ProjectID: f.proj.Meta.ID},
	}
	second.StampOwner()
	must(t, "upsert second account", f.stores.ServiceAccount.Upsert(ctx, second))
	return second
}

// twoBindingsOnTheAccount binds f.account in two role bindings, each beside
// another subject so both can let go of it; "aa-binding" is written first.
func (f policyDeleteFixture) twoBindingsOnTheAccount(t *testing.T, ctx context.Context) (*rolebinding.RoleBinding, *rolebinding.RoleBinding) {
	t.Helper()
	second := f.secondAccount(t, ctx)
	acct := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID}
	other := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: second.Meta.ID}
	r := f.customRole(t, ctx, "key-reader")
	return f.roleBinding(t, ctx, "aa-binding", r.Meta.ID, f.projectOwner(), acct, other),
		f.roleBinding(t, ctx, "zz-binding", r.Meta.ID, f.projectOwner(), acct, other)
}

func TestDetach_SecondCallChangesNothing(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	prov := f.provider(t, ctx, "twice-vendor")
	m := f.model(t, ctx, "twice-model", prov.Meta.ID)
	other := f.model(t, ctx, "twice-other", prov.Meta.ID)
	f.pricing(t, ctx, "twice-shared", m.Meta.ID, other.Meta.ID)
	f.pricing(t, ctx, "twice-only", m.Meta.ID)

	first := decodeDetach(t, f.detachRow(t, "models", m.Meta.ID, false))
	wantGroups(t, "first detached", first.Detached, wantGroup{"pricing", "spec.targetModels", true, 1})
	second := decodeDetach(t, f.detachRow(t, "models", m.Meta.ID, false))
	wantGroups(t, "second detached", second.Detached)
	if !slices.EqualFunc(first.Blockers, second.Blockers, func(a, b blockerGroup) bool {
		return a.Kind == b.Kind && a.Field == b.Field && a.Count == b.Count && slices.Equal(a.Items, b.Items)
	}) {
		t.Fatalf("blockers changed between calls: %+v then %+v", first.Blockers, second.Blockers)
	}
}

func TestDetachRole_OnlyAdminKeepsTheRole(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	admin := f.customRole(t, ctx, user.RoleAdmin)
	f.alice.Roles = []string{user.RoleAdmin}
	must(t, "make alice admin", f.users.Upsert(ctx, f.alice))

	body := decodeDetach(t, f.detachRow(t, "roles", admin.Meta.ID, false))
	wantGroups(t, "detached", body.Detached)
	wantGroups(t, "blockers", body.Blockers, wantGroup{"user", "roles", true, 1})
	got, err := f.users.Get(ctx, f.alice.ID)
	must(t, "get alice", err)
	if !slices.Equal(got.Roles, []string{user.RoleAdmin}) {
		t.Fatalf("alice roles = %v, want [admin]", got.Roles)
	}
}

func TestDetachRole_LeavesOneAdmin(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	admin := f.customRole(t, ctx, user.RoleAdmin)
	f.alice.Roles = []string{user.RoleAdmin}
	must(t, "make alice admin", f.users.Upsert(ctx, f.alice))
	bob := &user.User{ID: meta.NewID(), Username: "del-bob-" + meta.NewID()[:8], Roles: []string{user.RoleAdmin}}
	must(t, "upsert bob", f.users.Upsert(ctx, bob))

	body := decodeDetach(t, f.detachRow(t, "roles", admin.Meta.ID, false))
	wantGroups(t, "detached", body.Detached, wantGroup{"user", "roles", true, 1})
	wantGroups(t, "blockers", body.Blockers, wantGroup{"user", "roles", true, 1})
	admins := 0
	for _, id := range []string{f.alice.ID, bob.ID} {
		u, err := f.users.Get(ctx, id)
		must(t, "get user", err)
		if u.HasRole(user.RoleAdmin) {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("admins left = %d, want 1", admins)
	}
}

// Alice cannot see the project's binding at all: it does not let go, and it
// is counted, not named.
func TestDetach_LeavesRowsTheCallerCannotUpdate(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "alice-policy", meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID})
	hidden := f.policyBinding(t, ctx, "hidden-binding", pol.Meta.ID, f.accountOf(t, ctx, f.account))

	w := f.detachRow(t, "policies", pol.Meta.ID, true)
	body := decodeDetach(t, w)
	if strings.Contains(w.Body.String(), hidden.Meta.Name) || strings.Contains(w.Body.String(), hidden.Meta.ID) {
		t.Fatalf("detach body names a row the caller may not see: %s", w.Body.String())
	}
	wantGroups(t, "detached", body.Detached)
	wantGroups(t, "blockers", body.Blockers, wantGroup{"policy-binding", "spec.policyId", false, 1})
	if g := body.Blockers[0]; g.Hidden != 1 || len(g.Items) != 0 {
		t.Fatalf("binding blocker = %+v, want one hidden row and no items", g)
	}
}

// A write that fails partway rolls back the ones before it: the second role
// binding's update is made to fail.
func TestDetach_FailedWriteRollsEverythingBack(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	first, _ := f.twoBindingsOnTheAccount(t, ctx)
	_, err := f.pool.Exec(ctx, `
		CREATE FUNCTION fail_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.name = 'zz-binding' THEN RAISE EXCEPTION 'write refused by test'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_write BEFORE UPDATE ON role_bindings
		FOR EACH ROW EXECUTE FUNCTION fail_write();`)
	must(t, "install failing trigger", err)

	w := f.detachRow(t, "service-accounts", f.account.Meta.ID, false)
	wantStatus(t, w, http.StatusInternalServerError)
	if !strings.Contains(w.Body.String(), "write refused by test") {
		t.Fatalf("detach failed for another reason: %s", w.Body.String())
	}
	rb, err := f.stores.RoleBinding.Get(ctx, first.Meta.ID)
	must(t, "get first binding", err)
	if len(rb.Spec.Subjects) != 2 || rb.Meta.Dirty {
		t.Fatalf("first binding after failed detach = %+v, want untouched", rb.Spec.Subjects)
	}
}

// A concurrent change answers 409 conflict and rolls back like any failure.
// The trigger raises the serialization failure a racing writer would cause.
func TestDetach_ConcurrentChangeIsConflict(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	first, _ := f.twoBindingsOnTheAccount(t, ctx)
	_, err := f.pool.Exec(ctx, `
		CREATE FUNCTION fail_serialization() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.name = 'zz-binding' THEN RAISE EXCEPTION 'concurrent update' USING ERRCODE = 'serialization_failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_serialization BEFORE UPDATE ON role_bindings
		FOR EACH ROW EXECUTE FUNCTION fail_serialization();`)
	must(t, "install conflicting trigger", err)

	w := f.detachRow(t, "service-accounts", f.account.Meta.ID, false)
	wantStatus(t, w, http.StatusConflict)
	var body inUseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "conflict" {
		t.Fatalf("409 body = %s, want error.code conflict", w.Body.String())
	}
	rb, err := f.stores.RoleBinding.Get(ctx, first.Meta.ID)
	must(t, "get first binding", err)
	if len(rb.Spec.Subjects) != 2 || rb.Meta.Dirty {
		t.Fatalf("first binding after conflicting detach = %+v, want untouched", rb.Spec.Subjects)
	}
}

func TestDetach_AuditsEachModifiedRow(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	a, z := f.twoBindingsOnTheAccount(t, ctx)

	decodeDetach(t, f.detachRow(t, "service-accounts", f.account.Meta.ID, false))
	f.emitter.Close()

	updated := map[string][]string{}
	detached := 0
	for _, ev := range f.sink.all() {
		switch {
		case ev.Action == "role-bindings.update" && ev.Change != nil:
			updated[ev.Resource.ID] = ev.Change.Fields
		case ev.Action == "service-accounts.detach" && ev.Resource.ID == f.account.Meta.ID:
			detached++
		}
	}
	for _, rb := range []*rolebinding.RoleBinding{a, z} {
		if !slices.Equal(updated[rb.Meta.ID], []string{"spec.subjects"}) {
			t.Fatalf("audit for binding %s = %v, want [spec.subjects]; all: %v", rb.Meta.Name, updated[rb.Meta.ID], updated)
		}
	}
	if len(updated) != 2 || detached != 1 {
		t.Fatalf("audit rows: %d binding updates and %d detach rows, want 2 and 1", len(updated), detached)
	}
}

func TestDetach_SnapshotFollows(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	prov := f.provider(t, ctx, "snapshot-vendor")
	m := f.model(t, ctx, "snapshot-model", prov.Meta.ID)
	other := f.model(t, ctx, "snapshot-other", prov.Meta.ID)
	rates := f.pricing(t, ctx, "snapshot-rates", m.Meta.ID, other.Meta.ID)
	listener, err := f.cat.Hydrate(ctx, f.stores, appcatalog.BootstrapOptions{Pool: f.pool})
	must(t, "hydrate", err)
	runCatalogListener(t, ctx, f.cat, listener)
	if p, ok := f.cat.Current().Pricing(rates.Meta.ID); !ok || !slices.Contains(p.Spec.TargetModelIDs, m.Meta.ID) {
		t.Fatalf("snapshot pricing before detach = %+v (found %v), want the model in its targets", p, ok)
	}

	decodeDetach(t, f.detachRow(t, "models", m.Meta.ID, false))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, ok := f.cat.Current().Pricing(rates.Meta.ID); ok && !slices.Contains(p.Spec.TargetModelIDs, m.Meta.ID) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot did not drop the model from the pricing within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runCatalogListener runs l until the test ends and returns once LISTEN is
// attached, seen as the generation bump of the reload the listener runs then.
func runCatalogListener(t *testing.T, ctx context.Context, cat *appcatalog.Catalog, l *appcatalog.Listener) {
	t.Helper()
	gen := cat.Current().Generation()
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Run(lctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for cat.Current().Generation() == gen {
		if time.Now().After(deadline) {
			t.Fatal("listener did not attach LISTEN within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

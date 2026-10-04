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
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
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

func (f policyDeleteFixture) keyPolicy(t *testing.T, ctx context.Context, id string) *key.Key {
	t.Helper()
	k, err := f.stores.Key.Get(ctx, id)
	if err != nil || k == nil {
		t.Fatalf("get key %s: %v", id, err)
	}
	return k
}

func TestDetachPolicy_ReleasesKeysAndKeepsTierHostKey(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "detach-policy", f.projectOwner())
	ka := f.projectKey(t, ctx, "key-a", pol.Meta.ID)
	kb := f.projectKey(t, ctx, "key-b", pol.Meta.ID)
	hk := f.hostKey(t, ctx, "detach-host-key", pol.Meta.ID)

	body := decodeDetach(t, f.detachRow(t, "policies", pol.Meta.ID, false))
	wantGroups(t, "detached", body.Detached, wantGroup{"key", "spec.policyId", true, 2})
	wantGroups(t, "blockers", body.Blockers, wantGroup{"host-key", "spec.policyId", false, 1})

	for _, k := range []*key.Key{ka, kb} {
		got := f.keyPolicy(t, ctx, k.Meta.ID)
		if got.Spec.PolicyID != "" || !got.Meta.Dirty {
			t.Fatalf("key %s after detach: policyId=%q dirty=%v, want cleared and dirty", k.Meta.Name, got.Spec.PolicyID, got.Meta.Dirty)
		}
	}
	gotHK, err := f.stores.HostKey.Get(ctx, hk.Meta.ID)
	must(t, "get host key", err)
	if gotHK.Spec.PolicyID != pol.Meta.ID || gotHK.Meta.Dirty {
		t.Fatalf("host key after detach = %+v, want untouched", gotHK.Spec)
	}

	wantBlockers(t, f.deletePolicy(t, pol.Meta.ID, false), wantGroup{"host-key", "spec.policyId", false, 1})
	other := f.policy(t, ctx, "other-tier", f.projectOwner())
	gotHK.Spec.PolicyID = other.Meta.ID
	must(t, "reassign host key", f.stores.HostKey.Upsert(ctx, gotHK))
	wantStatus(t, f.deletePolicy(t, pol.Meta.ID, false), http.StatusNoContent)
}

// A list that needs an entry lets go of the model only while it holds another.
func TestDetachModel_PricingKeepsItsLastModel(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	prov := f.provider(t, ctx, "vendor")
	m := f.model(t, ctx, "vendor-model", prov.Meta.ID)
	other := f.model(t, ctx, "vendor-other", prov.Meta.ID)
	shared := f.pricing(t, ctx, "shared-rates", m.Meta.ID, other.Meta.ID)
	only := f.pricing(t, ctx, "only-rates", m.Meta.ID)
	pol := f.policy(t, ctx, "model-policy", f.projectOwner())
	pol.Spec.ModelIDs = []string{m.Meta.ID, other.Meta.ID}
	must(t, "upsert policy", f.stores.Policy.Upsert(ctx, pol))

	body := decodeDetach(t, f.detachRow(t, "models", m.Meta.ID, false))
	wantGroups(t, "detached", body.Detached,
		wantGroup{"policy", "spec.modelIds", true, 1},
		wantGroup{"pricing", "spec.targetModels", true, 1},
	)
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
	p, err := f.stores.Policy.Get(ctx, pol.Meta.ID)
	must(t, "get policy", err)
	if !slices.Equal(p.Spec.ModelIDs, []string{other.Meta.ID}) {
		t.Fatalf("policy models = %v, want only the other model", p.Spec.ModelIDs)
	}
}

// A binding lets go of the account only while it names another subject.
func TestDetachServiceAccount_BindingKeepsItsLastSubject(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	second := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "second-account"},
		Spec: serviceaccount.Spec{ProjectID: f.proj.Meta.ID},
	}
	second.StampOwner()
	must(t, "upsert second account", f.stores.ServiceAccount.Upsert(ctx, second))
	acct := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: f.account.Meta.ID}
	other := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: second.Meta.ID}
	r := f.customRole(t, ctx, "key-reader")
	shared := f.roleBinding(t, ctx, "shared-binding", r.Meta.ID, f.projectOwner(), acct, other)
	pol := f.policy(t, ctx, "bound-policy", f.projectOwner())
	only := f.policyBinding(t, ctx, "only-binding", pol.Meta.ID, acct)

	body := decodeDetach(t, f.detachRow(t, "service-accounts", f.account.Meta.ID, false))
	wantGroups(t, "detached", body.Detached, wantGroup{"role-binding", "spec.subjects", true, 1})
	wantGroups(t, "blockers", body.Blockers, wantGroup{"policy-binding", "spec.subjects", false, 1})
	rb, err := f.stores.RoleBinding.Get(ctx, shared.Meta.ID)
	must(t, "get role binding", err)
	if len(rb.Spec.Subjects) != 1 || rb.Spec.Subjects[0].ID != second.Meta.ID {
		t.Fatalf("role binding subjects = %+v, want only the second account", rb.Spec.Subjects)
	}
	pb, err := f.stores.PolicyBinding.Get(ctx, only.Meta.ID)
	must(t, "get policy binding", err)
	if len(pb.Spec.Subjects) != 1 || pb.Spec.Subjects[0].ID != f.account.Meta.ID || pb.Meta.Dirty {
		t.Fatalf("policy binding = %+v, want untouched", pb.Spec.Subjects)
	}
}

func TestDetach_SecondCallChangesNothing(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "twice-policy", f.projectOwner())
	f.projectKey(t, ctx, "key-a", pol.Meta.ID)
	f.hostKey(t, ctx, "twice-host-key", pol.Meta.ID)

	first := decodeDetach(t, f.detachRow(t, "policies", pol.Meta.ID, false))
	second := decodeDetach(t, f.detachRow(t, "policies", pol.Meta.ID, false))
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

// Alice may edit her own key but neither the shared host nor the project's
// key, which she cannot even see.
func TestDetach_LeavesRowsTheCallerCannotUpdate(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "alice-policy", meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID})
	own := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: f.alice.ID},
			PolicyID:  pol.Meta.ID,
			KeyHash:   strings.Repeat("c", 64), Prefix: "sk-alice",
		},
	}
	must(t, "upsert alice key", f.stores.Key.Upsert(ctx, own))
	hidden := f.projectKey(t, ctx, "key-h", pol.Meta.ID)
	f.host.Spec.Policies = []string{pol.Meta.ID}
	must(t, "upsert host", f.stores.Host.Upsert(ctx, f.host))

	w := f.detachRow(t, "policies", pol.Meta.ID, true)
	body := decodeDetach(t, w)
	if strings.Contains(w.Body.String(), hidden.Meta.Name) || strings.Contains(w.Body.String(), hidden.Meta.ID) {
		t.Fatalf("detach body names a row the caller may not see: %s", w.Body.String())
	}
	wantGroups(t, "detached", body.Detached, wantGroup{"key", "spec.policyId", true, 1})
	wantGroups(t, "blockers", body.Blockers,
		wantGroup{"host", "spec.policies", true, 1},
		wantGroup{"key", "spec.policyId", true, 1},
	)
	if g := body.Blockers[1]; g.Hidden != 1 || len(g.Items) != 0 {
		t.Fatalf("key blocker = %+v, want one hidden row and no items", g)
	}

	if got := f.keyPolicy(t, ctx, own.Meta.ID); got.Spec.PolicyID != "" {
		t.Fatalf("alice's key still on the policy")
	}
	if got := f.keyPolicy(t, ctx, hidden.Meta.ID); got.Spec.PolicyID != pol.Meta.ID || got.Meta.Dirty {
		t.Fatalf("hidden key after detach = %+v, want untouched", got)
	}
	h, err := f.stores.Host.Get(ctx, f.host.Meta.ID)
	must(t, "get host", err)
	if !slices.Equal(h.Spec.Policies, []string{pol.Meta.ID}) || h.Meta.Dirty {
		t.Fatalf("host after detach = %+v, want untouched", h.Spec)
	}
}

// A write that fails partway rolls back the ones before it: keys are written
// before service accounts, and the service-account update is made to fail.
func TestDetach_FailedWriteRollsEverythingBack(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "rollback-policy", f.projectOwner())
	k := f.projectKey(t, ctx, "key-a", pol.Meta.ID)
	f.account.Spec.PolicyID = pol.Meta.ID
	must(t, "upsert service account", f.stores.ServiceAccount.Upsert(ctx, f.account))
	_, err := f.pool.Exec(ctx, `
		CREATE FUNCTION fail_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'write refused by test'; END $$;
		CREATE TRIGGER fail_write BEFORE UPDATE ON service_accounts
		FOR EACH ROW EXECUTE FUNCTION fail_write();`)
	must(t, "install failing trigger", err)

	w := f.detachRow(t, "policies", pol.Meta.ID, false)
	wantStatus(t, w, http.StatusInternalServerError)
	if !strings.Contains(w.Body.String(), "write refused by test") {
		t.Fatalf("detach failed for another reason: %s", w.Body.String())
	}
	if got := f.keyPolicy(t, ctx, k.Meta.ID); got.Spec.PolicyID != pol.Meta.ID || got.Meta.Dirty {
		t.Fatalf("key after failed detach = %+v, want untouched", got)
	}
	sa, err := f.stores.ServiceAccount.Get(ctx, f.account.Meta.ID)
	must(t, "get service account", err)
	if sa.Spec.PolicyID != pol.Meta.ID {
		t.Fatalf("service account after failed detach = %+v, want untouched", sa.Spec)
	}
}

func TestDetach_AuditsEachModifiedRow(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "audited-policy", f.projectOwner())
	ka := f.projectKey(t, ctx, "key-a", pol.Meta.ID)
	kb := f.projectKey(t, ctx, "key-b", pol.Meta.ID)

	decodeDetach(t, f.detachRow(t, "policies", pol.Meta.ID, false))
	f.emitter.Close()

	updated := map[string][]string{}
	detached := 0
	for _, ev := range f.sink.all() {
		switch {
		case ev.Action == "keys.update" && ev.Change != nil:
			updated[ev.Resource.ID] = ev.Change.Fields
		case ev.Action == "policies.detach" && ev.Resource.ID == pol.Meta.ID:
			detached++
		}
	}
	for _, k := range []*key.Key{ka, kb} {
		if !slices.Equal(updated[k.Meta.ID], []string{"spec.policyId"}) {
			t.Fatalf("audit for key %s = %v, want [spec.policyId]; all: %v", k.Meta.Name, updated[k.Meta.ID], updated)
		}
	}
	if len(updated) != 2 || detached != 1 {
		t.Fatalf("audit rows: %d key updates and %d detach rows, want 2 and 1", len(updated), detached)
	}
}

func TestDetach_SnapshotFollows(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	pol := f.policy(t, ctx, "snapshot-policy", f.projectOwner())
	k := f.projectKey(t, ctx, "key-a", pol.Meta.ID)
	listener, err := f.cat.Hydrate(ctx, f.stores, appcatalog.BootstrapOptions{Pool: f.pool})
	must(t, "hydrate", err)
	runCatalogListener(t, ctx, f.cat, listener)
	if got, ok := f.cat.Current().Key(k.Meta.ID); !ok || got.Spec.PolicyID != pol.Meta.ID {
		t.Fatalf("snapshot key before detach = %+v (found %v), want it on the policy", got, ok)
	}

	decodeDetach(t, f.detachRow(t, "policies", pol.Meta.ID, false))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, ok := f.cat.Current().Key(k.Meta.ID); ok && got.Spec.PolicyID == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot did not drop the key's policy within 5s")
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

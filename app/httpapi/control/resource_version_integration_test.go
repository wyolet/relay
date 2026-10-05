//go:build integration

// resource_version_integration_test.go drives optimistic concurrency through
// the real handler and stores against Postgres: the version compare lives in
// the stores' upsert statements, which have no fake seam.

package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/role"
)

// licensedHandler mounts the control API over f's stores with custom roles
// licensed, so a user-owned role is editable through PUT.
func licensedHandler(t *testing.T, f policyDeleteFixture) http.Handler {
	t.Helper()
	deps := mountDeps(t)
	deps.Stores = f.stores
	deps.Users = f.users
	deps.Catalog = f.cat
	deps.Authz = audit.Authorizer{Inner: testRBAC()}
	deps.License = &fakeLicense{info: license.Info{Licensed: true, Features: []string{license.FeatureCustomRoles}}}
	r := chi.NewRouter()
	Mount(r, deps)
	return r
}

func adminCall(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// readRow GETs a row as the raw JSON object a form would hold.
func readRow(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	w := adminCall(t, h, http.MethodGet, path, nil)
	wantStatus(t, w, http.StatusOK)
	return decodeObject(t, w)
}

func decodeObject(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return row
}

func metadataOf(row map[string]any) map[string]any { return row["metadata"].(map[string]any) }

func versionOf(t *testing.T, row map[string]any) string {
	t.Helper()
	v, _ := metadataOf(row)["resourceVersion"].(string)
	if v == "" {
		t.Fatalf("row carries no metadata.resourceVersion: %v", row)
	}
	return v
}

func storedVersion(t *testing.T, m meta.Metadata) string {
	t.Helper()
	if m.ResourceVersion == "" {
		t.Fatalf("%s read back with no resource version", m.Name)
	}
	return m.ResourceVersion
}

// Two editors open the same role. B removes a rule; A's later save from the
// form it opened earlier must not bring the rule back.
func TestUpdateRefusesAStaleRoleEdit(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)
	h := licensedHandler(t, f)
	r := &role.Role{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "release-manager", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.alice.ID}},
		Spec: role.Spec{Rules: []role.Rule{
			{Kinds: []string{"keys"}, Verbs: []string{"get"}},
			{Kinds: []string{"policies"}, Verbs: []string{"update"}},
		}},
	}
	must(t, "upsert role", f.stores.Role.Upsert(ctx, r))
	path := "/roles/by-id/" + r.Meta.ID
	getPath := "/roles/" + r.Meta.ID

	editorA := readRow(t, h, getPath)
	editorB := readRow(t, h, getPath)
	opened := versionOf(t, editorA)
	if versionOf(t, editorB) != opened {
		t.Fatalf("two reads of an unchanged row disagree on the version")
	}

	editorB["spec"].(map[string]any)["rules"] = editorB["spec"].(map[string]any)["rules"].([]any)[:1]
	w := adminCall(t, h, http.MethodPut, path, editorB)
	wantStatus(t, w, http.StatusOK)
	if versionOf(t, decodeObject(t, w)) == opened {
		t.Fatalf("PUT response kept the version it replaced")
	}

	metadataOf(editorA)["description"] = "owns releases"
	w = adminCall(t, h, http.MethodPut, path, editorA)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale PUT status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var problem inUseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	if problem.Error.Code != "stale_resource_version" {
		t.Fatalf("error.code = %q, want stale_resource_version", problem.Error.Code)
	}

	got, err := f.stores.Role.Get(ctx, r.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("get role: %v", err)
	}
	if len(got.Spec.Rules) != 1 || got.Spec.Rules[0].Kinds[0] != "keys" {
		t.Fatalf("stale PUT brought the removed rule back: %+v", got.Spec.Rules)
	}
	if got.Meta.Description != "" {
		t.Fatalf("stale PUT landed its description: %q", got.Meta.Description)
	}

	// A PUT without a version is still accepted, and still bumps it.
	fresh := readRow(t, h, getPath)
	before := versionOf(t, fresh)
	delete(metadataOf(fresh), "resourceVersion")
	metadataOf(fresh)["description"] = "owns releases"
	w = adminCall(t, h, http.MethodPut, path, fresh)
	wantStatus(t, w, http.StatusOK)
	after := decodeObject(t, w)
	if versionOf(t, after) == before {
		t.Fatalf("unconditional PUT left the version unchanged")
	}
	if metadataOf(after)["description"] != "owns releases" {
		t.Fatalf("unconditional PUT did not apply: %v", after)
	}

	// List carries the same version as get.
	w = adminCall(t, h, http.MethodGet, "/roles", nil)
	wantStatus(t, w, http.StatusOK)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, it := range list.Items {
		if metadataOf(it)["id"] == r.Meta.ID && versionOf(t, it) != versionOf(t, after) {
			t.Fatalf("list version %v, get version %v", metadataOf(it)["resourceVersion"], versionOf(t, after))
		}
	}
}

// The compare is part of the store's upsert, not the handler's read: a write
// carrying a version the row has moved past changes nothing, and of two
// writers holding the same version exactly one lands.
func TestStoreUpsertComparesVersionAtomically(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)

	pol := f.policy(t, ctx, "versioned-policy", f.projectOwner())
	hk := f.hostKey(t, ctx, "versioned-host-key", "")
	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "versioned-group", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: group.Spec{MemberIDs: []string{f.alice.ID}}}
	must(t, "upsert group", f.stores.Group.Upsert(ctx, g))

	t.Run("policy", func(t *testing.T) {
		stale, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
		fresh, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
		fresh.Spec.HostKeyIDs = []string{hk.Meta.ID}
		must(t, "fresh upsert", f.stores.Policy.Upsert(ctx, fresh))
		stale.Spec.HostKeyIDs = nil
		stale.Spec.Enabled = new(bool)
		if err := f.stores.Policy.Upsert(ctx, stale); !errors.Is(err, meta.ErrStaleResourceVersion) {
			t.Fatalf("stale upsert err = %v, want ErrStaleResourceVersion", err)
		}
		got, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
		if len(got.Spec.HostKeyIDs) != 1 || !got.IsEnabled() {
			t.Fatalf("stale upsert changed the policy: hostKeyIds=%v enabled=%v", got.Spec.HostKeyIDs, got.IsEnabled())
		}
	})

	t.Run("group", func(t *testing.T) {
		stale, _ := f.stores.Group.Get(ctx, g.Meta.ID)
		fresh, _ := f.stores.Group.Get(ctx, g.Meta.ID)
		fresh.Spec.MemberIDs = nil
		must(t, "fresh upsert", f.stores.Group.Upsert(ctx, fresh))
		stale.Meta.Description = "stale"
		if err := f.stores.Group.Upsert(ctx, stale); !errors.Is(err, meta.ErrStaleResourceVersion) {
			t.Fatalf("stale upsert err = %v, want ErrStaleResourceVersion", err)
		}
		got, _ := f.stores.Group.Get(ctx, g.Meta.ID)
		if len(got.Spec.MemberIDs) != 0 || got.Meta.Description != "" {
			t.Fatalf("stale upsert restored the membership: %+v", got)
		}
	})

	t.Run("host key", func(t *testing.T) {
		stale, _ := f.stores.HostKey.Get(ctx, hk.Meta.ID)
		fresh, _ := f.stores.HostKey.Get(ctx, hk.Meta.ID)
		fresh.Meta.Description = "fresh"
		must(t, "fresh upsert", f.stores.HostKey.Upsert(ctx, fresh))
		stale.Meta.Description = "stale"
		if err := f.stores.HostKey.Upsert(ctx, stale); !errors.Is(err, meta.ErrStaleResourceVersion) {
			t.Fatalf("stale upsert err = %v, want ErrStaleResourceVersion", err)
		}
		got, _ := f.stores.HostKey.Get(ctx, hk.Meta.ID)
		if got.Meta.Description != "fresh" {
			t.Fatalf("stale upsert landed: %q", got.Meta.Description)
		}
	})

	t.Run("unparseable version", func(t *testing.T) {
		got, _ := f.stores.Group.Get(ctx, g.Meta.ID)
		got.Meta.ResourceVersion = "not-a-version"
		if err := f.stores.Group.Upsert(ctx, got); !errors.Is(err, meta.ErrStaleResourceVersion) {
			t.Fatalf("err = %v, want ErrStaleResourceVersion", err)
		}
	})

	t.Run("concurrent writers", func(t *testing.T) {
		base, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
		var wg sync.WaitGroup
		errs := make([]error, 8)
		start := make(chan struct{})
		for i := range errs {
			p, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
			if p.Meta.ResourceVersion != base.Meta.ResourceVersion {
				t.Fatalf("row moved before the race started")
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = f.stores.Policy.Upsert(ctx, p)
			}()
		}
		close(start)
		wg.Wait()
		landed := 0
		for _, err := range errs {
			switch {
			case err == nil:
				landed++
			case !errors.Is(err, meta.ErrStaleResourceVersion):
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if landed != 1 {
			t.Fatalf("%d writers holding the same version landed, want 1", landed)
		}
	})
}

// Every write to a row moves its version, whichever path made it.
func TestEveryWritePathChangesTheVersion(t *testing.T) {
	f, ctx := newPolicyDeleteFixture(t)

	t.Run("unconditional upsert", func(t *testing.T) {
		pol := f.policy(t, ctx, "upsert-policy", f.projectOwner())
		before, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
		next := &policy.Policy{Meta: meta.Metadata{ID: pol.Meta.ID, Name: pol.Meta.Name, Owner: pol.Meta.Owner}}
		must(t, "upsert", f.stores.Policy.Upsert(ctx, next))
		after, _ := f.stores.Policy.Get(ctx, pol.Meta.ID)
		if storedVersion(t, after.Meta) == storedVersion(t, before.Meta) {
			t.Fatalf("unconditional upsert left the version at %s", after.Meta.ResourceVersion)
		}
	})

	t.Run("detach", func(t *testing.T) {
		pol := f.policy(t, ctx, "detach-version-policy", f.projectOwner())
		k := f.projectKey(t, ctx, "detach-key-c", pol.Meta.ID)
		before := f.keyPolicy(t, ctx, k.Meta.ID)
		decodeDetach(t, f.detachRow(t, "policies", pol.Meta.ID, false))
		after := f.keyPolicy(t, ctx, k.Meta.ID)
		if after.Spec.PolicyID != "" {
			t.Fatalf("detach left the key on the policy")
		}
		if storedVersion(t, after.Meta) == storedVersion(t, before.Meta) {
			t.Fatalf("detach left the key's version at %s", after.Meta.ResourceVersion)
		}
	})

	t.Run("credential status", func(t *testing.T) {
		hk := f.hostKey(t, ctx, "status-host-key", "")
		before, _ := f.stores.HostKey.Get(ctx, hk.Meta.ID)
		must(t, "set status", f.stores.HostKey.SetCredentialStatus(ctx, hk.Meta.ID, hostkey.CredentialStatus{}))
		after, _ := f.stores.HostKey.Get(ctx, hk.Meta.ID)
		if storedVersion(t, after.Meta) == storedVersion(t, before.Meta) {
			t.Fatalf("status write left the version at %s", after.Meta.ResourceVersion)
		}
	})

	t.Run("built-in role seed", func(t *testing.T) {
		must(t, "seed", role.SeedBuiltins(ctx, f.stores.Role, nil, nil))
		roles, err := f.stores.Role.List(ctx)
		must(t, "list roles", err)
		var viewer *role.Role
		for _, r := range roles {
			if role.IsBuiltin(r.Meta.Name) {
				viewer = r
				break
			}
		}
		if viewer == nil {
			t.Fatal("seed wrote no built-in role")
		}
		before := storedVersion(t, viewer.Meta)
		viewer.Spec.Rules = []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}
		viewer.Meta.ResourceVersion = ""
		must(t, "drift the built-in", f.stores.Role.Upsert(ctx, viewer))
		must(t, "re-seed", role.SeedBuiltins(ctx, f.stores.Role, nil, nil))
		after, _ := f.stores.Role.Get(ctx, viewer.Meta.ID)
		if v := storedVersion(t, after.Meta); v == before {
			t.Fatalf("seed rewrite left the version at %s", v)
		}
	})
}

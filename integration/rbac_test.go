//go:build integration

// rbac_test.go covers the Role / RoleBinding / PolicyBinding control-plane
// surface end to end: the built-in role seed, the CRUD routes and their
// guards, the list filters, the PG cascades that drop a deleted principal
// from every binding, and the seed loader's ordering.
package integration_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

type bindingRow struct {
	Metadata struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"owner"`
	} `json:"metadata"`
	Spec struct {
		RoleID   string `json:"roleId"`
		Priority int    `json:"priority"`
		Scope    struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"scope"`
		Subjects []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"subjects"`
	} `json:"spec"`
}

type bindingList struct {
	Items []bindingRow `json:"items"`
	Total int          `json:"total"`
}

// seedBuiltinRoles runs the built-in seed and returns the row named name.
func seedBuiltinRoles(t *testing.T, st *stack) map[string]*role.Role {
	t.Helper()
	ctx := context.Background()
	if err := role.SeedBuiltins(ctx, st.stores.Role, slog.Default(), nil); err != nil {
		t.Fatalf("seed built-in roles: %v", err)
	}
	roles, err := st.stores.Role.List(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	byName := map[string]*role.Role{}
	for _, r := range roles {
		byName[r.Meta.Name] = r
	}
	return byName
}

func TestIntegration_BuiltinRoleSeed(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()

	roles := seedBuiltinRoles(t, st)
	if len(roles) != 7 {
		t.Fatalf("seeded %d roles, want 7", len(roles))
	}
	admin, ok := roles["admin"]
	if !ok {
		t.Fatal("no admin role seeded")
	}
	if !admin.Allows("teams", "delete") {
		t.Error("seeded admin role should allow every verb")
	}

	// A second boot changes nothing: same ids, same count.
	if err := role.SeedBuiltins(ctx, st.stores.Role, slog.Default(), nil); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	again, err := st.stores.Role.List(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(again) != 7 {
		t.Fatalf("after the second seed there are %d roles, want 7", len(again))
	}
	for _, r := range again {
		if r.Meta.ID != roles[r.Meta.Name].Meta.ID {
			t.Errorf("role %q id changed on the second seed", r.Meta.Name)
		}
	}

	// Built-in names are reserved, and the reserved check runs before the
	// license gate.
	code, raw := st.adminDo(http.MethodPost, "/api/roles",
		`{"metadata":{"name":"admin"},"spec":{"rules":[{"kinds":["keys"],"verbs":["get"]}]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /api/roles named admin = %d: %s", code, raw)
	}

	// A custom role needs a license; this deployment has none.
	code, raw = st.adminDo(http.MethodPost, "/api/roles",
		`{"metadata":{"name":"release-manager"},"spec":{"rules":[{"kinds":["keys"],"verbs":["get"]}]}}`)
	if code != http.StatusForbidden {
		t.Fatalf("POST /api/roles unlicensed = %d: %s", code, raw)
	}

	// System-owned rows are never edited through generic CRUD.
	code, raw = st.adminDo(http.MethodPut, "/api/roles/by-id/"+admin.Meta.ID,
		`{"metadata":{"name":"admin"},"spec":{"rules":[{"kinds":["keys"],"verbs":["get"]}]}}`)
	if code != http.StatusForbidden {
		t.Fatalf("PUT on a system role = %d: %s", code, raw)
	}
}

func TestIntegration_BindingCRUD(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)

	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := st.users.Upsert(ctx, alice); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	code, raw := st.adminDo(http.MethodPost, "/api/teams", `{"metadata":{"name":"platform"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/teams = %d: %s", code, raw)
	}
	var team tenancyRow
	if err := json.Unmarshal(raw, &team); err != nil {
		t.Fatalf("decode team: %v", err)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/projects",
		`{"metadata":{"name":"ml-search"},"spec":{"teamId":"`+team.Metadata.ID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/projects = %d: %s", code, raw)
	}
	var project tenancyRow
	if err := json.Unmarshal(raw, &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"ml-search-default"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}
	var pol tenancyRow
	if err := json.Unmarshal(raw, &pol); err != nil {
		t.Fatalf("decode policy: %v", err)
	}

	// Team-scoped binding: the owner mirrors the scope.
	body := `{"metadata":{"name":"platform-admins"},"spec":{"roleId":"` + roles["team-admin"].Meta.ID + `",` +
		`"scope":{"kind":"team","id":"` + team.Metadata.ID + `"},` +
		`"subjects":[{"kind":"group","name":"platform-eng"},{"kind":"user","id":"` + alice.ID + `"}]}}`
	code, raw = st.adminDo(http.MethodPost, "/api/role-bindings", body)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/role-bindings = %d: %s", code, raw)
	}
	var scoped bindingRow
	if err := json.Unmarshal(raw, &scoped); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if scoped.Metadata.Owner.Kind != "team" || scoped.Metadata.Owner.ID != team.Metadata.ID {
		t.Errorf("owner = %+v, want the team scope", scoped.Metadata.Owner)
	}
	if len(scoped.Spec.Subjects) != 2 || scoped.Spec.Subjects[0].Name != "platform-eng" {
		t.Errorf("subjects = %+v", scoped.Spec.Subjects)
	}

	// Global binding: the system scope carries no id.
	body = `{"metadata":{"name":"everyone-viewer"},"spec":{"roleId":"` + roles["viewer"].Meta.ID + `",` +
		`"scope":{"kind":"system"},"subjects":[{"kind":"group","name":"system:authenticated"}]}}`
	code, raw = st.adminDo(http.MethodPost, "/api/role-bindings", body)
	if code != http.StatusCreated {
		t.Fatalf("POST global role-binding = %d: %s", code, raw)
	}
	var global bindingRow
	if err := json.Unmarshal(raw, &global); err != nil {
		t.Fatalf("decode global binding: %v", err)
	}
	if global.Metadata.Owner.Kind != "system" || global.Metadata.Owner.ID != "" {
		t.Errorf("global owner = %+v, want {system}", global.Metadata.Owner)
	}

	// Unknown role, and a user subject that is not a user.
	code, raw = st.adminDo(http.MethodPost, "/api/role-bindings",
		`{"metadata":{"name":"ghost-role"},"spec":{"roleId":"0195f8a0-0000-7000-8000-0000000000fd",`+
			`"scope":{"kind":"system"},"subjects":[{"kind":"group","name":"platform-eng"}]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST with an unknown role = %d: %s", code, raw)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/role-bindings",
		`{"metadata":{"name":"ghost-user"},"spec":{"roleId":"`+roles["viewer"].Meta.ID+`",`+
			`"scope":{"kind":"system"},"subjects":[{"kind":"user","id":"0195f8a0-0000-7000-8000-0000000000fe"}]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST with a non-user subject = %d: %s", code, raw)
	}

	// Policy binding without a priority takes the default.
	body = `{"metadata":{"name":"ml-search-everyone"},"spec":{"projectId":"` + project.Metadata.ID + `",` +
		`"policyId":"` + pol.Metadata.ID + `","subjects":[{"kind":"group","name":"system:authenticated"}]}}`
	code, raw = st.adminDo(http.MethodPost, "/api/policy-bindings", body)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policy-bindings = %d: %s", code, raw)
	}
	var pb bindingRow
	if err := json.Unmarshal(raw, &pb); err != nil {
		t.Fatalf("decode policy binding: %v", err)
	}
	if pb.Spec.Priority != 100 {
		t.Errorf("priority = %d, want the default 100", pb.Spec.Priority)
	}
	if pb.Metadata.Owner.Kind != "project" || pb.Metadata.Owner.ID != project.Metadata.ID {
		t.Errorf("owner = %+v, want the project", pb.Metadata.Owner)
	}

	// Filters.
	code, raw = st.adminDo(http.MethodGet, "/api/role-bindings?subject=group:platform-eng", "")
	if code != http.StatusOK {
		t.Fatalf("GET ?subject = %d: %s", code, raw)
	}
	var list bindingList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || list.Items[0].Metadata.Name != "platform-admins" {
		t.Errorf("?subject=group:platform-eng returned %+v", list.Items)
	}

	code, raw = st.adminDo(http.MethodGet,
		"/api/role-bindings?scope_kind=team&scope_id="+team.Metadata.ID, "")
	if code != http.StatusOK {
		t.Fatalf("GET ?scope_kind = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || list.Items[0].Metadata.Name != "platform-admins" {
		t.Errorf("?scope_kind=team returned %+v", list.Items)
	}

	code, raw = st.adminDo(http.MethodGet, "/api/policy-bindings?project_id="+project.Metadata.ID, "")
	if code != http.StatusOK {
		t.Fatalf("GET ?project_id = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || list.Items[0].Metadata.Name != "ml-search-everyone" {
		t.Errorf("?project_id returned %+v", list.Items)
	}
}

func TestIntegration_BindingCascades(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)

	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := st.users.Upsert(ctx, alice); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	code, raw := st.adminDo(http.MethodPost, "/api/teams", `{"metadata":{"name":"platform"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/teams = %d: %s", code, raw)
	}
	var team tenancyRow
	if err := json.Unmarshal(raw, &team); err != nil {
		t.Fatalf("decode team: %v", err)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/projects",
		`{"metadata":{"name":"ml-search"},"spec":{"teamId":"`+team.Metadata.ID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/projects = %d: %s", code, raw)
	}
	var project tenancyRow
	if err := json.Unmarshal(raw, &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	sa := &serviceaccount.ServiceAccount{}
	sa.Meta.ID = ids.New()
	sa.Meta.Name = "search-indexer"
	sa.Spec.ProjectID = project.Metadata.ID
	if err := st.stores.ServiceAccount.Upsert(ctx, sa); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}

	body := `{"metadata":{"name":"platform-admins"},"spec":{"roleId":"` + roles["team-admin"].Meta.ID + `",` +
		`"scope":{"kind":"team","id":"` + team.Metadata.ID + `"},` +
		`"subjects":[{"kind":"user","id":"` + alice.ID + `"},` +
		`{"kind":"serviceaccount","id":"` + sa.Meta.ID + `"},` +
		`{"kind":"group","name":"platform-eng"}]}}`
	code, raw = st.adminDo(http.MethodPost, "/api/role-bindings", body)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/role-bindings = %d: %s", code, raw)
	}
	var b bindingRow
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode binding: %v", err)
	}

	// Deleting the user drops it from the binding's subjects.
	if err := st.users.Delete(ctx, alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	stored, err := st.stores.RoleBinding.Get(ctx, b.Metadata.ID)
	if err != nil || stored == nil {
		t.Fatalf("get binding: %v", err)
	}
	for _, sub := range stored.Spec.Subjects {
		if sub.ID == alice.ID {
			t.Fatalf("deleted user survived in the binding: %+v", stored.Spec.Subjects)
		}
	}
	if len(stored.Spec.Subjects) != 2 {
		t.Errorf("subjects = %+v, want the service account and the group", stored.Spec.Subjects)
	}

	// Deleting the role drops its bindings.
	if err := st.stores.Role.Delete(ctx, roles["team-admin"].Meta.ID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	gone, err := st.stores.RoleBinding.Get(ctx, b.Metadata.ID)
	if err != nil {
		t.Fatalf("get binding after the role delete: %v", err)
	}
	if gone != nil {
		t.Error("binding survived its role's delete")
	}
}

// ── seed ordering ────────────────────────────────────────────────────────

const seedRBACYAML = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata:
  name: platform
  owner: {kind: system}
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata:
  name: ml-search
spec:
  team: platform
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata:
  name: ml-search-pol
  owner: {kind: project, name: ml-search}
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Group
metadata:
  name: data-science
  owner: {kind: system}
spec:
  members: [alice]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: ServiceAccount
metadata:
  name: search-indexer
spec:
  project: ml-search
  policy: ml-search-pol
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Role
metadata:
  name: release-manager
  owner: {kind: system}
spec:
  rules:
    - kinds: [keys, service-accounts]
      verbs: [get, list, create]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata:
  name: platform-releases
spec:
  role: release-manager
  scope: {kind: team, name: platform}
  subjects:
    - {kind: group, name: data-science}
    - {kind: user, name: alice}
    - {kind: serviceaccount, name: search-indexer}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: PolicyBinding
metadata:
  name: ml-search-everyone
spec:
  project: ml-search
  policy: ml-search-pol
  subjects:
    - {kind: group, name: system:authenticated}
`

func TestIntegration_SeedRBAC(t *testing.T) {
	st := newStack(t)
	pool := testPool(t)
	ctx := context.Background()

	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := st.users.Upsert(ctx, alice); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rbac.yaml"), []byte(seedRBACYAML), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	res, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if res.Roles != 1 || res.RoleBindings != 1 || res.PolicyBindings != 1 {
		t.Fatalf("seeded %+v, want 1 role / 1 role binding / 1 policy binding", res)
	}

	bindings, err := st.stores.RoleBinding.List(ctx)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("list role bindings: %v %d", err, len(bindings))
	}
	b := bindings[0]
	if b.Meta.Owner != b.Spec.Scope {
		t.Errorf("owner %+v does not mirror scope %+v", b.Meta.Owner, b.Spec.Scope)
	}
	if len(b.Spec.Subjects) != 3 {
		t.Fatalf("subjects = %+v", b.Spec.Subjects)
	}
	if b.Spec.Subjects[0].Name != "data-science" || b.Spec.Subjects[0].ID != "" {
		t.Errorf("group subject = %+v, want a name only", b.Spec.Subjects[0])
	}
	if b.Spec.Subjects[1].ID != alice.ID {
		t.Errorf("user subject = %+v, want alice's id", b.Spec.Subjects[1])
	}

	pbs, err := st.stores.PolicyBinding.List(ctx)
	if err != nil || len(pbs) != 1 {
		t.Fatalf("list policy bindings: %v %d", err, len(pbs))
	}
	if pbs[0].EffectivePriority() != 100 {
		t.Errorf("priority = %d, want 100", pbs[0].EffectivePriority())
	}

	// A second run over the same tree is a no-op for unchanged rows and
	// skips anything an operator has since edited.
	if _, err := st.adminUpdateDirty(b.Meta.ID); err != nil {
		t.Fatalf("dirty the binding: %v", err)
	}
	again, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir})
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if again.Skipped == 0 {
		t.Error("second seed skipped nothing; the dirty row should have been preserved")
	}
}

// adminUpdateDirty marks a role binding operator-edited by writing it
// through the control API, which is what sets the dirty flag.
func (s *stack) adminUpdateDirty(id string) (int, error) {
	b, err := s.stores.RoleBinding.Get(context.Background(), id)
	if err != nil || b == nil {
		return 0, err
	}
	body, err := json.Marshal(b)
	if err != nil {
		return 0, err
	}
	code, raw := s.adminDo(http.MethodPut, "/api/role-bindings/by-id/"+id, string(body))
	if code != http.StatusOK {
		s.t.Fatalf("PUT /api/role-bindings/by-id/%s = %d: %s", id, code, raw)
	}
	return code, nil
}

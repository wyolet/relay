//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Pruning a team takes the bindings scoped to it and to its projects, even ones the bundle never named: their scope is gone, so they would apply to a later team or project given the id.
func TestIntegration_ApplyPruneDeletesBindingsAtPrunedScopes(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	tenancy := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: alpha, owner: {kind: system}, labels: {managed: gitops}}\nspec: {}\n" +
		"---\napiVersion: relay.wyolet.dev/v1alpha2\nkind: Project\nmetadata: {name: search, labels: {managed: gitops}}\nspec: {team: alpha}\n"
	keep := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: beta, owner: {kind: system}, labels: {managed: gitops}}\nspec: {}\n"
	if code, _, raw := st.applyBundle(tenancy+"---\n"+keep, ""); code != http.StatusOK {
		t.Fatalf("seed bundle: %d %s", code, raw)
	}
	teams, err := st.stores.Team.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, tm := range teams {
		ids[tm.Meta.Name] = tm.Meta.ID
	}
	projects, err := st.stores.Project.List(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects = %d, %v", len(projects), err)
	}
	userID := st.seedLogin(t, "dev", "pw-dev")
	st.bindRole(t, "alpha-viewers", roles["viewer"].Meta.ID, userID, "team", ids["alpha"])
	st.bindRole(t, "search-viewers", roles["viewer"].Meta.ID, userID, "project", projects[0].Meta.ID)
	st.bindRole(t, "beta-viewers", roles["viewer"].Meta.ID, userID, "team", ids["beta"])

	// A bundle that keeps a binding at a scope it prunes is refused before anything is written.
	declared := "---\napiVersion: relay.wyolet.dev/v1alpha2\nkind: RoleBinding\nmetadata: {name: search-readers, owner: {kind: project, name: search}}\n" +
		"spec: {role: viewer, scope: {kind: project, name: search}, subjects: [{kind: user, name: dev}]}\n"
	if code, _, raw := st.applyBundle(keep+declared, "prune=true&selector=managed=gitops"); code != http.StatusBadRequest || !strings.Contains(string(raw), "pruned in the same apply") {
		t.Fatalf("prune keeping a binding at the pruned project = %d, want 400: %s", code, raw)
	}
	if got, err := st.stores.Team.Get(ctx, ids["alpha"]); err != nil || got == nil {
		t.Fatalf("the refused prune deleted the team: %v", err)
	}

	code, plan, raw := st.applyBundle(keep, "prune=true&selector=managed=gitops")
	if code != http.StatusOK || plan.action("Team", "alpha") != "delete" || plan.action("Project", "search") != "delete" {
		t.Fatalf("prune = %d %+v: %s", code, plan.Plan, raw)
	}
	for name, want := range map[string]int{"alpha-viewers": http.StatusNotFound, "search-viewers": http.StatusNotFound, "beta-viewers": http.StatusOK} {
		if code, raw := st.adminDo(http.MethodGet, "/api/role-bindings/"+name, ""); code != want {
			t.Errorf("role binding %s after the prune = %d, want %d: %s", name, code, want, raw)
		}
	}
}

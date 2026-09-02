//go:build integration

// bughunt5_test.go covers the round-5 authorization fixes end to end under
// RELAY_AUTHZ=rbac: role-binding escalation, the open scoped lists, a
// project binding reading its parent team, and a personal key allowed onto
// a project-owned policy.
package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// A team admin must not be able to bind a role wider than the one they
// hold — binding the built-in `admin` at their own team would make them the
// deployment's admin. The same rule applies through apply.
func TestIntegration_RoleBindingEscalation(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	adminID := st.seedLogin(t, "tadmin", "pw-tadmin")
	st.bindRole(t, "alpha-admins", roles["team-admin"].Meta.ID, adminID, "team", teamA)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	admin := st.login(t, "tadmin", "pw-tadmin")
	devID := st.seedLogin(t, "dev", "pw-dev")

	body := func(name, roleID string) string {
		return `{"metadata":{"name":"` + name + `","owner":{"kind":"team","id":"` + teamA + `"}},` +
			`"spec":{"roleId":"` + roleID + `","scope":{"kind":"team","id":"` + teamA + `"},` +
			`"subjects":[{"kind":"user","id":"` + devID + `"}]}}`
	}

	code, raw := admin.do(http.MethodPost, "/api/role-bindings", body("alpha-escalate", roles["admin"].Meta.ID))
	if code != http.StatusForbidden {
		t.Fatalf("team-admin binding the admin role = %d, want 403: %s", code, raw)
	}
	code, raw = admin.do(http.MethodPost, "/api/role-bindings", body("alpha-more-admins", roles["team-admin"].Meta.ID))
	if code != http.StatusCreated {
		t.Fatalf("team-admin binding their own role at their own team = %d, want 201: %s", code, raw)
	}
	// The global admin is exempt.
	code, raw = st.adminDo(http.MethodPost, "/api/role-bindings", body("alpha-real-admin", roles["admin"].Meta.ID))
	if code != http.StatusCreated {
		t.Fatalf("admin binding the admin role = %d, want 201: %s", code, raw)
	}

	// Through apply the answer is the same 403.
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	bundle := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: RoleBinding\n" +
		"metadata:\n  name: alpha-applied-escalation\n  owner: {kind: team, name: alpha}\n" +
		"spec:\n  role: admin\n  scope: {kind: team, name: alpha}\n" +
		"  subjects:\n    - kind: user\n      name: dev\n"
	code, raw = admin.doAs(http.MethodPost, "/api/apply", bundle, "application/yaml")
	if code != http.StatusForbidden {
		t.Fatalf("apply of an escalating binding = %d, want 403: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodGet, "/api/role-bindings/alpha-applied-escalation", ""); code != http.StatusNotFound {
		t.Fatalf("the refused apply wrote a row: %d %s", code, raw)
	}
}

// An upgraded deployment has no bindings yet: every scoped list must answer
// an empty 200 rather than 403, and a project-bound developer must be able
// to read the team their project belongs to.
func TestIntegration_ScopedListsAndParentTeamReads(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamA)
	st.seedLogin(t, "nobody", "pw-nobody")
	devID := st.seedLogin(t, "dev", "pw-dev")
	st.bindRole(t, "alpha-devs", roles["developer"].Meta.ID, devID, "project", projA)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	nobody := st.login(t, "nobody", "pw-nobody")
	for _, path := range []string{"/api/keys", "/api/policies", "/api/projects", "/api/teams", "/api/service-accounts"} {
		code, raw := nobody.do(http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s with no bindings = %d, want 200: %s", path, code, raw)
		}
		var list struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if list.Total != 0 {
			t.Errorf("GET %s returned %d rows to a caller with no bindings", path, list.Total)
		}
	}
	// users stays global-only.
	if code, raw := nobody.do(http.MethodGet, "/api/users", ""); code != http.StatusForbidden {
		t.Fatalf("users list without a global binding = %d, want 403: %s", code, raw)
	}

	dev := st.login(t, "dev", "pw-dev")
	if code, raw := dev.do(http.MethodGet, "/api/teams/alpha", ""); code != http.StatusOK {
		t.Fatalf("project-bound developer reading the parent team = %d, want 200: %s", code, raw)
	}
}

// A developer's personal key may name their project's policy: they may
// create keys there, and the key then carries the project. Someone outside
// the project is still refused.
func TestIntegration_PersonalKeyOnAProjectPolicy(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamA)
	devID := st.seedLogin(t, "dev", "pw-dev")
	outsiderID := st.seedLogin(t, "outsider", "pw-outsider")
	st.bindRole(t, "alpha-devs", roles["developer"].Meta.ID, devID, "project", projA)

	code, raw := st.adminDo(http.MethodPost, "/api/policies",
		`{"metadata":{"name":"alpha-pol","owner":{"kind":"project","id":"`+projA+`"}},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}
	var pol tenancyRow
	if err := json.Unmarshal(raw, &pol); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	dev := st.login(t, "dev", "pw-dev")
	body := func(name, userID string) string {
		return `{"metadata":{"name":"` + name + `"},` +
			`"spec":{"principal":{"kind":"user","id":"` + userID + `"},"policyId":"` + pol.Metadata.ID + `"}}`
	}
	if code, raw := dev.do(http.MethodPost, "/api/keys", body("dev-personal", devID)); code != http.StatusCreated {
		t.Fatalf("developer's personal key on their project's policy = %d, want 201: %s", code, raw)
	}

	outsider := st.login(t, "outsider", "pw-outsider")
	if code, raw := outsider.do(http.MethodPost, "/api/keys", body("outsider-personal", outsiderID)); code == http.StatusCreated {
		t.Fatalf("a personal key outside the project was accepted: %s", raw)
	}
}

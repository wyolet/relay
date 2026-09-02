//go:build integration

package integration_test

import (
	"context"
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

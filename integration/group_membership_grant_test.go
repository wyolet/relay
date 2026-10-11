//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
)

// A member of a group acts under every binding naming it, so adding one takes the grant check of those bindings. The caller may edit groups everywhere but holds team-admin only in team alpha: it may fill a group bound within alpha, not one bound to admin globally.
func TestIntegration_GroupMembershipIsAGrant(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	editor := &role.Role{Meta: meta.Metadata{ID: meta.NewID(), Name: "group-editor", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{
			{Kinds: []string{"groups"}, Verbs: []string{"get", "list", "create", "update"}},
			{Kinds: []string{"system"}, Verbs: []string{"apply"}},
		}}}
	if err := st.stores.Role.Upsert(ctx, editor); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	teamA := st.mkTeam(t, "alpha")
	leadID := st.seedLogin(t, "lead", "pw-lead")
	devID := st.seedLogin(t, "dev", "pw-dev")
	st.bindRole(t, "alpha-admins", roles["team-admin"].Meta.ID, leadID, "team", teamA)
	st.bindRole(t, "group-editors", editor.Meta.ID, leadID, "system", "")

	bindGroup := func(name, roleID, scope, group string) {
		t.Helper()
		body := `{"metadata":{"name":"` + name + `","owner":` + scope + `},"spec":{"roleId":"` + roleID + `","scope":` + scope + `,` +
			`"subjects":[{"kind":"group","name":"` + group + `"}]}}`
		if code, raw := st.adminDo(http.MethodPost, "/api/role-bindings", body); code != http.StatusCreated {
			t.Fatalf("POST /api/role-bindings %s = %d: %s", name, code, raw)
		}
	}
	bindGroup("ops-admins", roles["admin"].Meta.ID, `{"kind":"system"}`, "ops-admins")
	bindGroup("alpha-ops", roles["team-admin"].Meta.ID, `{"kind":"team","id":"`+teamA+`"}`, "alpha-ops")
	mkGroup := func(name string) string {
		t.Helper()
		code, raw := st.adminDo(http.MethodPost, "/api/groups", `{"metadata":{"name":"`+name+`"},"spec":{}}`)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/groups %s = %d: %s", name, code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode group: %v", err)
		}
		return row.Metadata.ID
	}
	opsAdmins, alphaOps := mkGroup("ops-admins"), mkGroup("alpha-ops")
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	lead := st.login(t, "lead", "pw-lead")
	withDev := func(name string) string {
		return `{"metadata":{"name":"` + name + `"},"spec":{"memberIds":["` + devID + `"]}}`
	}

	if code, raw := lead.do(http.MethodPut, "/api/groups/by-id/"+opsAdmins, withDev("ops-admins")); code != http.StatusForbidden {
		t.Fatalf("adding a member to a group bound to admin globally = %d, want 403: %s", code, raw)
	}
	if g, err := st.stores.Group.Get(ctx, opsAdmins); err != nil || len(g.Spec.MemberIDs) != 0 {
		t.Fatalf("the refused add was written: %+v %v", g, err)
	}
	if code, raw := lead.do(http.MethodPost, "/api/groups", withDev("ops-admins-2")); code != http.StatusCreated {
		t.Fatalf("creating an unbound group with a member = %d, want 201: %s", code, raw)
	}
	if code, raw := lead.do(http.MethodPut, "/api/groups/by-id/"+alphaOps, withDev("alpha-ops")); code != http.StatusOK {
		t.Fatalf("adding a member to a group bound within the caller's team = %d, want 200: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPut, "/api/groups/by-id/"+opsAdmins, withDev("ops-admins")); code != http.StatusOK {
		t.Fatalf("an admin adding the member = %d, want 200: %s", code, raw)
	}
	// Removing a member needs no grant.
	if code, raw := lead.do(http.MethodPut, "/api/groups/by-id/"+opsAdmins, `{"metadata":{"name":"ops-admins"},"spec":{}}`); code != http.StatusOK {
		t.Fatalf("removing a member = %d, want 200: %s", code, raw)
	}
	// Apply refuses the same group while planning, so the rest of the bundle is not written either; a binding may name a group before it exists.
	bindGroup("root-ops", roles["admin"].Meta.ID, `{"kind":"system"}`, "root-ops")
	bundle := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Group\nmetadata: {name: harmless, owner: {kind: system}}\nspec: {members: [dev]}\n" +
		"---\napiVersion: relay.wyolet.dev/v1alpha2\nkind: Group\nmetadata: {name: root-ops, owner: {kind: system}}\nspec: {members: [dev]}\n"
	code, raw := lead.doAs(http.MethodPost, "/api/apply", bundle, "application/yaml")
	if code != http.StatusForbidden || strings.Contains(string(raw), `"applied"`) {
		t.Fatalf("apply creating a globally bound group with a member = %d, want a 403 plan refusal: %s", code, raw)
	}
	for _, name := range []string{"harmless", "root-ops"} {
		if code, raw := st.adminDo(http.MethodGet, "/api/groups/"+name, ""); code != http.StatusNotFound {
			t.Fatalf("the refused apply wrote group %s: %d %s", name, code, raw)
		}
	}

	// The probes skip the audit wrapper, so the refused request is recorded as the group update it was.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var refused []audit.Event
		for _, ev := range st.auditList("?limit=1000").Events {
			if ev.Outcome.Code == http.StatusForbidden && ev.Request.Method == http.MethodPut {
				refused = append(refused, ev)
			}
		}
		if len(refused) == 1 {
			if ev := refused[0]; ev.Action != "groups.update" || ev.Resource.ID != opsAdmins {
				t.Fatalf("audit row of the refused add = %+v, want groups.update on the group", ev)
			}
			break
		}
		if len(refused) > 1 || time.Now().After(deadline) {
			t.Fatalf("refused PUT audit rows = %+v, want exactly one", refused)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

// A user who creates a team or project through the API administers it, and so has a member account there; the admin token and apply create no binding.
func TestCreatorAdministersWhatTheyCreate(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	ctx := context.Background()
	ownerID := st.seedLogin(t, "owner", "pw-owner")
	st.bindRole(t, "owner-admin", roles["admin"].Meta.ID, ownerID, "system", "")
	leadID := st.seedLogin(t, "lead", "pw-lead")
	if err := st.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	bindingsAt := func(scope meta.Owner) []*rolebinding.RoleBinding {
		t.Helper()
		all, err := st.stores.RoleBinding.List(ctx)
		if err != nil {
			t.Fatalf("list role bindings: %v", err)
		}
		var out []*rolebinding.RoleBinding
		for _, b := range all {
			if b.Spec.Scope == scope {
				out = append(out, b)
			}
		}
		return out
	}
	created := func(code int, raw []byte) string {
		t.Helper()
		if code != http.StatusCreated {
			t.Fatalf("create = %d: %s", code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return row.Metadata.ID
	}
	wantAdmin := func(scope meta.Owner, roleName, userID string) {
		t.Helper()
		got := bindingsAt(scope)
		if len(got) != 1 || got[0].Spec.RoleID != roles[roleName].Meta.ID ||
			!slices.Equal(got[0].Spec.Subjects, []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: userID}}) {
			t.Fatalf("bindings at %s %s = %+v, want one %s binding of the creator", scope.Kind, scope.ID, got, roleName)
		}
		st.accountAt(t, "member:"+string(scope.Kind)+":"+scope.ID+":"+userID)
	}

	owner := st.login(t, "owner", "pw-owner")
	teamID := created(owner.do(http.MethodPost, "/api/teams", `{"metadata":{"name":"platform","displayName":"Platform"},"spec":{}}`))
	team := meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	wantAdmin(team, "team-admin", ownerID)

	// A team admin's project makes them its admin too.
	st.bindRole(t, "lead-team-admin", roles["team-admin"].Meta.ID, leadID, "team", teamID)
	if err := st.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	lead := st.login(t, "lead", "pw-lead")
	projectID := created(lead.do(http.MethodPost, "/api/projects", `{"metadata":{"name":"web","displayName":"Web"},"spec":{"teamId":"`+teamID+`"}}`))
	wantAdmin(meta.Owner{Kind: meta.OwnerProject, ID: projectID}, "project-admin", leadID)

	// The creator is listed with admin in both, through the team binding for the project.
	if err := st.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	code, raw := lead.do(http.MethodGet, "/api/me/contexts", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/me/contexts = %d: %s", code, raw)
	}
	var contexts []struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Admin bool   `json:"admin"`
	}
	if err := json.Unmarshal(raw, &contexts); err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 3 || contexts[0].Kind != "personal" || contexts[1].ID != teamID || !contexts[1].Admin || contexts[2].ID != projectID || !contexts[2].Admin {
		t.Errorf("lead's contexts = %+v, want personal, the team and the project, admin in both", contexts)
	}

	tokenTeam := meta.Owner{Kind: meta.OwnerTeam, ID: created(st.adminDo(http.MethodPost, "/api/teams", `{"metadata":{"name":"by-token"},"spec":{}}`))}
	if got := bindingsAt(tokenTeam); len(got) != 0 {
		t.Errorf("the admin token's team got bindings %+v", got)
	}

	code, raw = owner.doAs(http.MethodPost, "/api/apply", "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: applied, owner: {kind: system}}\nspec: {}\n", "application/yaml")
	if code != http.StatusOK {
		t.Fatalf("apply a team = %d: %s", code, raw)
	}
	applied, err := st.stores.Team.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range applied {
		if tm.Meta.Name == "applied" {
			if got := bindingsAt(meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}); len(got) != 0 {
				t.Errorf("the applied team got bindings %+v", got)
			}
		}
	}
}

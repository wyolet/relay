//go:build integration

// policy_binding_owners_test.go drives policy-binding owners through the control API under RELAY_AUTHZ=rbac: a team admin binds within their team, only a system-scope grant makes a system-owned binding, and subjects and policies outside the owner's scope are refused.

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

func TestIntegration_PolicyBindingOwners(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	teamA, teamB := st.mkTeam(t, "owners-a"), st.mkTeam(t, "owners-b")
	projA, projB := st.mkProject(t, "owners-a-one", teamA), st.mkProject(t, "owners-b-one", teamB)
	saB := st.mkServiceAccount(t, "owners-b-sa", projB)
	hst := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "owners-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	if err := st.stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	mkPolicy := func(name, owner string) string {
		code, raw := st.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"`+name+`"`+owner+`},"spec":{}}`)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/policies %s = %d: %s", name, code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode policy: %v", err)
		}
		return row.Metadata.ID
	}
	shared := mkPolicy("owners-shared", "")
	siblings := mkPolicy("owners-b-policy", `,"owner":{"kind":"team","id":"`+teamB+`"}`)

	leadID := st.seedLogin(t, "owners-lead", "pw-owners-lead")
	st.bindRole(t, "owners-a-admins", roles["team-admin"].Meta.ID, leadID, "team", teamA)
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	lead := st.login(t, "owners-lead", "pw-owners-lead")

	binding := func(name, owner, policyID, subject string) string {
		return `{"metadata":{"name":"` + name + `","owner":` + owner + `},"spec":{"policyId":"` + policyID + `","subjects":[` + subject + `]}}`
	}
	system := `{"kind":"system"}`
	ownTeam := `{"kind":"team","id":"` + teamA + `"}`
	ownProject := `{"kind":"project","id":"` + projA + `"}`
	projectA := `{"kind":"project","id":"` + projA + `"}`
	host := `{"kind":"host","id":"` + hst.Meta.ID + `"}`

	for _, tc := range []struct {
		name      string
		as        func(method, path, body string) (int, []byte)
		body      string
		want      int
		wantOwner string
	}{
		{"a team admin binds within the team", lead.do, binding("owners-team", ownTeam, shared, projectA), http.StatusCreated, "team"},
		{"a team admin cannot make a system binding", lead.do, binding("owners-sys-lead", system, shared, `{"kind":"team","id":"`+teamA+`"}`), http.StatusForbidden, ""},
		{"the operator binds a host from the system", st.adminDo, binding("owners-sys-host", system, shared, host), http.StatusCreated, "system"},
		{"a project binding names a host", st.adminDo, binding("owners-project-host", ownProject, shared, host), http.StatusBadRequest, ""},
		{"a project binding names another project's service account", st.adminDo, binding("owners-project-sa", ownProject, shared, `{"kind":"serviceaccount","id":"`+saB+`"}`), http.StatusBadRequest, ""},
		{"a team binding uses a sibling team's policy", st.adminDo, binding("owners-sibling", ownTeam, siblings, `{"kind":"team","id":"`+teamA+`"}`), http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, raw := tc.as(http.MethodPost, "/api/policy-bindings", tc.body)
			if code != tc.want {
				t.Fatalf("POST = %d, want %d: %s", code, tc.want, raw)
			}
			if tc.wantOwner == "" {
				return
			}
			var row tenancyRow
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatalf("decode binding: %v", err)
			}
			if row.Metadata.Owner.Kind != tc.wantOwner {
				t.Fatalf("owner = %+v, want %s", row.Metadata.Owner, tc.wantOwner)
			}
		})
	}
}

//go:build integration

// policy_binding_owners_test.go drives policy-binding owners through the control API under RELAY_AUTHZ=rbac: a team admin binds within their team, the organization's policies are bound by an admin, and accounts and policies outside the owner's scope are refused.

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
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
	orgs := map[string]string{}
	for _, name := range []string{"owners-org", "owners-other-org"} {
		org := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: hst.Meta.ID}}
		if err := st.stores.Account.Upsert(ctx, org); err != nil {
			t.Fatalf("upsert provider account: %v", err)
		}
		orgs[name] = org.Meta.ID
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
	ownPolicy := mkPolicy("owners-a-policy", `,"owner":{"kind":"team","id":"`+teamA+`"}`)
	siblings := mkPolicy("owners-b-policy", `,"owner":{"kind":"team","id":"`+teamB+`"}`)

	leadID := st.seedLogin(t, "owners-lead", "pw-owners-lead")
	st.bindRole(t, "owners-a-admins", roles["team-admin"].Meta.ID, leadID, "team", teamA)
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	lead := st.login(t, "owners-lead", "pw-owners-lead")

	binding := func(name, owner, policyID, accountID string) string {
		return `{"metadata":{"name":"` + name + `","owner":` + owner + `},"spec":{"policyId":"` + policyID + `","accountIds":["` + accountID + `"]}}`
	}
	system := `{"kind":"system"}`
	ownTeam := `{"kind":"team","id":"` + teamA + `"}`
	ownProject := `{"kind":"project","id":"` + projA + `"}`
	projectA, teamAccount := st.accountAt(t, "project:"+projA), st.accountAt(t, "team:"+teamA)

	for _, tc := range []struct {
		name      string
		as        func(method, path, body string) (int, []byte)
		body      string
		want      int
		wantOwner string
	}{
		{"a team admin binds within the team", lead.do, binding("owners-team", ownTeam, ownPolicy, projectA), http.StatusCreated, "team"},
		// The admin token's policy is the organization's, which only an admin sees.
		{"a team admin cannot bind the organization's policy", lead.do, binding("owners-team-org", ownTeam, shared, teamAccount), http.StatusNotFound, ""},
		{"the operator binds the organization's policy in a team", st.adminDo, binding("owners-team-shared", ownTeam, shared, teamAccount), http.StatusCreated, "team"},
		{"a team admin cannot make a global binding", lead.do, binding("owners-sys-lead", system, ownPolicy, teamAccount), http.StatusBadRequest, ""},
		{"the operator binds a provider account globally", st.adminDo, binding("owners-sys-org", system, shared, orgs["owners-org"]), http.StatusCreated, "organization"},
		{"a project binding names a provider account it does not own", st.adminDo, binding("owners-project-org", ownProject, shared, orgs["owners-other-org"]), http.StatusBadRequest, ""},
		{"a project binding names another project's service account", st.adminDo, binding("owners-project-sa", ownProject, shared, st.accountAt(t, "service:"+saB)), http.StatusBadRequest, ""},
		{"a team binding uses a sibling team's policy", st.adminDo, binding("owners-sibling", ownTeam, siblings, teamAccount), http.StatusBadRequest, ""},
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

// The organization's policies are an admin's to bind: a project admin does not see them, and even an admin cannot bind one linking another team's pool in the project, which would put the project's traffic on that team's credentials.
func TestIntegration_PolicyBindingRefusesAnotherTeamsPool(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	teamA, teamB := st.mkTeam(t, "borrow-a"), st.mkTeam(t, "borrow-b")
	projA := st.mkProject(t, "borrow-a-one", teamA)
	hst := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "borrow-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	if err := st.stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	create := func(path, body string) string {
		code, raw := st.adminDo(http.MethodPost, path, body)
		if code != http.StatusCreated {
			t.Fatalf("POST %s = %d: %s", path, code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return row.Metadata.ID
	}
	poolB := create("/api/pools", `{"metadata":{"name":"borrow-b-pool","owner":{"kind":"team","id":"`+teamB+`"}},"spec":{"hostId":"`+hst.Meta.ID+`"}}`)
	borrowing := create("/api/policies", `{"metadata":{"name":"borrow-shared"},"spec":{"poolIds":["`+poolB+`"]}}`)
	plain := create("/api/policies", `{"metadata":{"name":"borrow-plain"},"spec":{}}`)

	adminID := st.seedLogin(t, "borrow-admin", "pw-borrow-admin")
	st.bindRole(t, "borrow-a-admins", roles["project-admin"].Meta.ID, adminID, "project", projA)
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	admin := st.login(t, "borrow-admin", "pw-borrow-admin")
	projAccount := st.accountAt(t, "project:"+projA)
	binding := func(name, policyID string) string {
		return `{"metadata":{"name":"` + name + `","owner":{"kind":"project","id":"` + projA + `"}},"spec":{"policyId":"` + policyID + `","accountIds":["` + projAccount + `"]}}`
	}
	for _, id := range []string{borrowing, plain} {
		if code, raw := admin.do(http.MethodPost, "/api/policy-bindings", binding("borrow-hidden", id)); code != http.StatusNotFound {
			t.Fatalf("a project admin binding an organization policy = %d, want 404: %s", code, raw)
		}
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/policy-bindings", binding("borrow-pool", borrowing)); code != http.StatusBadRequest || !strings.Contains(string(raw), "borrow-b-pool") {
		t.Fatalf("binding an organization policy on another team's pool = %d, want 400 naming the pool: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/policy-bindings", binding("borrow-plain", plain)); code != http.StatusCreated {
		t.Fatalf("binding an organization policy without links = %d, want 201: %s", code, raw)
	}
}

// A binding created without metadata.owner takes the default other kinds do: the organization for the admin token, the caller's own user owner for a signed-in user.
func TestIntegration_PolicyBindingOmittedOwnerDefaults(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	teamID := st.mkTeam(t, "default-owner")
	aliceID := st.seedLogin(t, "default-alice", "pw-default-alice")
	alice := st.login(t, "default-alice", "pw-default-alice")
	create := func(as func(method, path, body string) (int, []byte), path, body string) tenancyRow {
		t.Helper()
		code, raw := as(http.MethodPost, path, body)
		if code != http.StatusCreated {
			t.Fatalf("POST %s = %d: %s", path, code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return row
	}
	shared := create(st.adminDo, "/api/policies", `{"metadata":{"name":"default-shared"},"spec":{}}`)
	own := create(alice.do, "/api/policies", `{"metadata":{"name":"default-alice-own"},"spec":{}}`)

	for _, tc := range []struct {
		name             string
		as               func(method, path, body string) (int, []byte)
		policyID         string
		accountID        string
		wantKind, wantID string
	}{
		{"the admin token", st.adminDo, shared.Metadata.ID, st.accountAt(t, "team:"+teamID), "organization", ""},
		{"a signed-in user", alice.do, own.Metadata.ID, st.accountAt(t, "personal:"+aliceID), "user", aliceID},
	} {
		row := create(tc.as, "/api/policy-bindings", `{"metadata":{"name":"default-`+strings.ReplaceAll(tc.name, " ", "-")+`"},"spec":{"policyId":"`+tc.policyID+`","accountIds":["`+tc.accountID+`"]}}`)
		if row.Metadata.Owner.Kind != tc.wantKind || row.Metadata.Owner.ID != tc.wantID {
			t.Errorf("%s: owner = %+v, want %s/%s", tc.name, row.Metadata.Owner, tc.wantKind, tc.wantID)
		}
	}
}

package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/meta"
)

// serviceAccountHarness serves service accounts as the control API registers them, under an authorizer that allows every signed-in caller.
func serviceAccountHarness(t *testing.T, seed ...*serviceaccount.ServiceAccount) (http.Handler, *memStore[serviceaccount.ServiceAccount]) {
	t.Helper()
	sameta := func(s *serviceaccount.ServiceAccount) *meta.Metadata { return &s.Meta }
	store := newMemStore(sameta, seed...)
	r, api := testAPI(t, scopeActors)
	mountKind[serviceaccount.ServiceAccount](api, "service-accounts", "service-account", store, authz.AlwaysAllowAuthenticated{}, sameta,
		(*serviceaccount.ServiceAccount).Validate, meta.OwnerProject, guardServiceAccount(Deps{Authz: authz.AlwaysAllowAuthenticated{}}), noSettings{})
	return r, store
}

func decodeServiceAccount(t *testing.T, body []byte) serviceaccount.ServiceAccount {
	t.Helper()
	var sa serviceaccount.ServiceAccount
	if err := json.Unmarshal(body, &sa); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return sa
}

// A create names the owner in metadata.owner, a project or a team; one that names only spec.projectId is owned by that project, as before team owners existed.
func TestCreateServiceAccountForEitherOwner(t *testing.T) {
	teamID, projectID, otherID := meta.NewID(), meta.NewID(), meta.NewID()
	project, team := meta.Owner{Kind: meta.OwnerProject, ID: projectID}, meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	h, _ := serviceAccountHarness(t)
	for _, tc := range []struct {
		name, body string
		owner      meta.Owner
		spec       serviceaccount.Spec
	}{
		{"spec.projectId alone", `{"metadata":{"name":"ci"},"spec":{"projectId":"` + projectID + `"}}`, project, serviceaccount.Spec{ProjectID: projectID}},
		{"a project owner", `{"metadata":{"name":"ci-2","owner":{"kind":"project","id":"` + projectID + `"}},"spec":{}}`, project, serviceaccount.Spec{ProjectID: projectID}},
		{"a project owner and its spec.projectId", `{"metadata":{"name":"ci-3","owner":{"kind":"project","id":"` + projectID + `"}},"spec":{"projectId":"` + projectID + `"}}`, project, serviceaccount.Spec{ProjectID: projectID}},
		{"a team owner", `{"metadata":{"name":"deployer","owner":{"kind":"team","id":"` + teamID + `"}},"spec":{}}`, team, serviceaccount.Spec{TeamID: teamID}},
		{"a team owner and a spec.teamId sent back", `{"metadata":{"name":"deployer-2","owner":{"kind":"team","id":"` + teamID + `"}},"spec":{"teamId":"` + otherID + `"}}`, team, serviceaccount.Spec{TeamID: teamID}},
	} {
		w := scopeReq(t, h, "alice", http.MethodPost, "/service-accounts", tc.body)
		if w.Code != http.StatusCreated {
			t.Errorf("%s: create = %d %s, want 201", tc.name, w.Code, w.Body)
			continue
		}
		got := decodeServiceAccount(t, w.Body.Bytes())
		if diff := cmp.Diff(tc.owner, got.Meta.Owner); diff != "" {
			t.Errorf("%s: owner (-want +got):\n%s", tc.name, diff)
		}
		if diff := cmp.Diff(tc.spec, got.Spec); diff != "" {
			t.Errorf("%s: spec (-want +got):\n%s", tc.name, diff)
		}
	}

	for _, tc := range []struct{ name, body, want string }{
		{"a project owner spec.projectId contradicts", `{"metadata":{"name":"x","owner":{"kind":"project","id":"` + otherID + `"}},"spec":{"projectId":"` + projectID + `"}}`,
			`serviceaccount "x": owner.id "` + otherID + `" does not match spec.projectId "` + projectID + `"`},
		{"a team owner and a spec.projectId", `{"metadata":{"name":"x","owner":{"kind":"team","id":"` + teamID + `"}},"spec":{"projectId":"` + projectID + `"}}`,
			`serviceaccount "x": owner is team "` + teamID + `" but spec.projectId names project "` + projectID + `"; a service account has one owner`},
		{"a team owner without an id", `{"metadata":{"name":"x","owner":{"kind":"team"}},"spec":{}}`, `serviceaccount "x": a team owner needs metadata.owner.id`},
		{"no owner at all", `{"metadata":{"name":"x"},"spec":{}}`, `serviceaccount "x": a project owner needs metadata.owner.id or spec.projectId`},
		{"a user owner", `{"metadata":{"name":"x","owner":{"kind":"user"}},"spec":{"projectId":"` + projectID + `"}}`, `serviceaccount "x": owner.kind must be project or team, got "user"`},
	} {
		wantAPIError(t, scopeReq(t, h, "alice", http.MethodPost, "/service-accounts", tc.body), http.StatusBadRequest, "", tc.want)
	}
}

// A service account keeps the owner it was made under: a team's stays the team's, and a project's never moves to a team.
func TestUpdateServiceAccountKeepsItsOwner(t *testing.T) {
	teamID, otherTeamID, projectID := meta.NewID(), meta.NewID(), meta.NewID()
	team := meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	inTeam := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: team}, Spec: serviceaccount.Spec{TeamID: teamID}}
	inProject := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "ci", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: serviceaccount.Spec{ProjectID: projectID}}
	h, store := serviceAccountHarness(t, inTeam, inProject)
	teamPath, projectPath := "/service-accounts/by-id/"+inTeam.Meta.ID, "/service-accounts/by-id/"+inProject.Meta.ID

	w := scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","displayName":"Deployer"},"spec":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d %s, want 200", w.Code, w.Body)
	}
	if got := decodeServiceAccount(t, w.Body.Bytes()); got.Meta.Owner != team || got.Spec != (serviceaccount.Spec{TeamID: teamID}) {
		t.Errorf("renamed team service account = owner %+v spec %+v, want its team kept", got.Meta.Owner, got.Spec)
	}

	inUse := func(name, kind string) string {
		return fmt.Sprintf("service account %q stays in the %s it was made in: its account and keys belong there; create a new service account in the other team or project", name, kind)
	}
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","owner":{"kind":"team","id":"`+otherTeamID+`"}},"spec":{}}`),
		http.StatusConflict, "resource_in_use", inUse("deployer", "team"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","owner":{"kind":"project","id":"`+projectID+`"}},"spec":{"projectId":"`+projectID+`"}}`),
		http.StatusConflict, "resource_in_use", inUse("deployer", "team"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, projectPath, `{"metadata":{"name":"ci","owner":{"kind":"team","id":"`+teamID+`"}},"spec":{}}`),
		http.StatusConflict, "resource_in_use", inUse("ci", "project"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer"},"spec":{"projectId":"`+projectID+`"}}`),
		http.StatusBadRequest, "", `serviceaccount "deployer": owner is team "`+teamID+`" but spec.projectId names project "`+projectID+`"; a service account has one owner`)

	if got := store.items[inTeam.Meta.ID]; got.Meta.Owner != team {
		t.Errorf("stored team service account moved to %+v", got.Meta.Owner)
	}
	if got := store.items[inProject.Meta.ID]; got.Meta.Owner != inProject.Meta.Owner {
		t.Errorf("stored project service account moved to %+v", got.Meta.Owner)
	}
}

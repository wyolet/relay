package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/auth/directory"
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

// refusingStore is a store whose delete the directory's foreign keys refuse, as a team delete is when a service account of the team appeared after the references scan.
type refusingStore struct{ *memStore[scopedThing] }

func (refusingStore) Delete(context.Context, string) error {
	return fmt.Errorf("delete team: %w: Key (id) is still referenced from table \"service_accounts\"", directory.ErrInvalid)
}

// A delete the store refuses for a row the references scan missed answers with the scan's 409, not a 404 carrying the store's text.
func TestDeleteRefusedByTheStoreAnswersInUse(t *testing.T) {
	row := &scopedThing{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}}
	scans := 0
	inUse := func(_ context.Context, kind, _, name string) (*droppedLinks, error) {
		if scans++; scans == 1 {
			return nil, nil
		}
		return nil, &resourceInUseError{APIError: httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: "resource_in_use",
			Message: kind + " " + name + " is referenced by 1 row(s); see blockers"}, HTTPStatus: http.StatusConflict},
			Blockers: []blockerGroup{{Kind: "service-account", Field: "metadata.owner.id", Count: 1}}}
	}
	r, api := testAPI(t, scopeActors)
	store := refusingStore{newMemStore(thingMeta, row)}
	registerKind[scopedThing](api, "things", "thing", store, authz.AlwaysAllowAuthenticated{}, thingMeta, nil, meta.OwnerUser,
		listScanResolver[scopedThing](store, thingMeta), nil, nil, nil, inUse, nil, noSettings{}, false, nil, nil, nil)
	wantAPIError(t, scopeReq(t, r, "alice", http.MethodDelete, "/things/by-id/"+row.Meta.ID, ""), http.StatusConflict, "resource_in_use",
		"thing platform is referenced by 1 row(s); see blockers")
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
		{"a team owner and a spec.teamId that is no id", `{"metadata":{"name":"deployer-3","owner":{"kind":"team","id":"` + teamID + `"}},"spec":{"teamId":"platform"}}`, team, serviceaccount.Spec{TeamID: teamID}},
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
		{"a user owner and a spec.projectId", `{"metadata":{"name":"x","owner":{"kind":"user"}},"spec":{"projectId":"` + projectID + `"}}`,
			`serviceaccount "x": owner is user "u-alice" but spec.projectId names project "` + projectID + `"; a service account has one owner`},
		{"another user as the owner", `{"metadata":{"name":"x","owner":{"kind":"user","id":"u-bob"}},"spec":{}}`, `owner.id must be empty or match the calling user`},
	} {
		wantAPIError(t, scopeReq(t, h, "alice", http.MethodPost, "/service-accounts", tc.body), http.StatusBadRequest, "", tc.want)
	}
}

// A user creates a service account they own, their personal bot; the admin token may create one for any user, a user only for themselves.
func TestCreateUserServiceAccount(t *testing.T) {
	h, _ := serviceAccountHarness(t)
	for _, tc := range []struct {
		name, who, body string
		owner           meta.Owner
	}{
		{"the caller's own", "alice", `{"metadata":{"name":"alice-bot","owner":{"kind":"user"}},"spec":{}}`, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}},
		{"the caller's own by id", "bob", `{"metadata":{"name":"bob-bot","owner":{"kind":"user","id":"u-bob"}},"spec":{}}`, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}},
		{"another user's by the admin token", "token", `{"metadata":{"name":"bob-bot-2","owner":{"kind":"user","id":"u-bob"}},"spec":{}}`, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}},
	} {
		w := scopeReq(t, h, tc.who, http.MethodPost, "/service-accounts", tc.body)
		if w.Code != http.StatusCreated {
			t.Errorf("%s: create = %d %s, want 201", tc.name, w.Code, w.Body)
			continue
		}
		got := decodeServiceAccount(t, w.Body.Bytes())
		if diff := cmp.Diff(serviceaccount.Spec{UserID: tc.owner.ID}, got.Spec); diff != "" || got.Meta.Owner != tc.owner {
			t.Errorf("%s: owner %+v, spec (-want +got):\n%s", tc.name, got.Meta.Owner, diff)
		}
	}
}

// A service account keeps the owner it was made under: a team's stays the team's, and a project's never moves to a team.
func TestUpdateServiceAccountKeepsItsOwner(t *testing.T) {
	teamID, otherTeamID, projectID := meta.NewID(), meta.NewID(), meta.NewID()
	team := meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	inTeam := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: team}, Spec: serviceaccount.Spec{TeamID: teamID}}
	inProject := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "ci", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: serviceaccount.Spec{ProjectID: projectID}}
	ofAlice := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}, Spec: serviceaccount.Spec{UserID: "u-alice"}}
	h, store := serviceAccountHarness(t, inTeam, inProject, ofAlice)
	teamPath, projectPath := "/service-accounts/by-id/"+inTeam.Meta.ID, "/service-accounts/by-id/"+inProject.Meta.ID

	w := scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","displayName":"Deployer"},"spec":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d %s, want 200", w.Code, w.Body)
	}
	if got := decodeServiceAccount(t, w.Body.Bytes()); got.Meta.Owner != team || got.Spec != (serviceaccount.Spec{TeamID: teamID}) {
		t.Errorf("renamed team service account = owner %+v spec %+v, want its team kept", got.Meta.Owner, got.Spec)
	}
	w = scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer"},"spec":{"teamId":"platform"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update sending back a spec.teamId that is no id = %d %s, want 200: the field is ignored on write", w.Code, w.Body)
	}
	if got := decodeServiceAccount(t, w.Body.Bytes()); got.Spec.TeamID != teamID {
		t.Errorf("spec.teamId after an update sending another = %q, want the team's id", got.Spec.TeamID)
	}

	inUse := func(name, kind string) string {
		return fmt.Sprintf("service account %q stays with the %s it was made under: its account and keys belong there; create a new service account for the other owner", name, kind)
	}
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","owner":{"kind":"team","id":"`+otherTeamID+`"}},"spec":{}}`),
		http.StatusConflict, "resource_in_use", inUse("deployer", "team"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","owner":{"kind":"project","id":"`+projectID+`"}},"spec":{"projectId":"`+projectID+`"}}`),
		http.StatusConflict, "resource_in_use", inUse("deployer", "team"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, projectPath, `{"metadata":{"name":"ci","owner":{"kind":"team","id":"`+teamID+`"}},"spec":{}}`),
		http.StatusConflict, "resource_in_use", inUse("ci", "project"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, "/service-accounts/by-id/"+ofAlice.Meta.ID, `{"metadata":{"name":"alice-bot","owner":{"kind":"team","id":"`+teamID+`"}},"spec":{}}`),
		http.StatusConflict, "resource_in_use", inUse("alice-bot", "user"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer","owner":{"kind":"user"}},"spec":{}}`),
		http.StatusConflict, "resource_in_use", inUse("deployer", "team"))
	wantAPIError(t, scopeReq(t, h, "alice", http.MethodPut, teamPath, `{"metadata":{"name":"deployer"},"spec":{"projectId":"`+projectID+`"}}`),
		http.StatusBadRequest, "", `serviceaccount "deployer": owner is team "`+teamID+`" but spec.projectId names project "`+projectID+`"; a service account has one owner`)

	if got := store.items[inTeam.Meta.ID]; got.Meta.Owner != team {
		t.Errorf("stored team service account moved to %+v", got.Meta.Owner)
	}
	if got := store.items[inProject.Meta.ID]; got.Meta.Owner != inProject.Meta.Owner {
		t.Errorf("stored project service account moved to %+v", got.Meta.Owner)
	}
}

//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

// A key's lane passes the same checks on create and on update: the user's personal account, a member account a role binding grants and a provider account the user owns take it; a member account opened on first use, a team's provider account, another user's account and a closed account do not.
func TestIntegration_KeyLaneChecksOnCreateAndUpdate(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	teamID := st.mkTeam(t, "lanes")
	granted, firstUse, closed := st.mkProject(t, "lanes-granted", teamID), st.mkProject(t, "lanes-first-use", teamID), st.mkProject(t, "lanes-closed", teamID)
	aliceID, bobID := st.seedUser("lanes-alice"), st.seedUser("lanes-bob")
	st.bindRole(t, "lanes-alice-dev", roles["developer"].Meta.ID, aliceID, "project", granted)

	hst := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "lanes-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	mustUpsert(t, st.stores.Host.Upsert(ctx, hst), "host")
	org := func(name string, owner meta.Owner) string {
		a := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner}, Spec: account.Spec{HostID: hst.Meta.ID}}
		mustUpsert(t, st.stores.Account.Upsert(ctx, a), "provider account")
		return a.Meta.ID
	}
	member := func(projectID string, close bool) string {
		id, err := st.stores.Account.EnsureManaged(ctx, account.Member(aliceID, "lanes-alice", meta.Owner{Kind: meta.OwnerProject, ID: projectID}, projectID, ""))
		if err != nil {
			t.Fatalf("open member account: %v", err)
		}
		if close {
			mustUpsert(t, st.stores.Account.SetMembership(ctx, id, true, true), "close member account")
		}
		return id
	}
	lanes := map[string]struct {
		id   string
		want int
	}{
		"the personal account":                 {st.accountAt(t, "personal:"+aliceID), http.StatusOK},
		"a member account a role grants":       {st.accountAt(t, "member:project:"+granted+":"+aliceID), http.StatusOK},
		"a provider account the user owns":     {org("lanes-alice-org", meta.Owner{Kind: meta.OwnerUser, ID: aliceID}), http.StatusOK},
		"a member account opened on first use": {member(firstUse, false), http.StatusForbidden},
		"a team's provider account":            {org("lanes-team-org", meta.Owner{Kind: meta.OwnerTeam, ID: teamID}), http.StatusForbidden},
		"another user's personal account":      {st.accountAt(t, "personal:"+bobID), http.StatusForbidden},
		"a closed member account":              {member(closed, true), http.StatusBadRequest},
	}
	principal := `"principal":{"kind":"user","id":"` + aliceID + `"}`

	code, raw := st.adminDo(http.MethodPost, "/api/keys", `{"metadata":{"name":"lanes-key"},"spec":{`+principal+`}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}
	var created struct {
		Key struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
		} `json:"key"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.Key.Metadata.ID == "" {
		t.Fatalf("decode key: %v %s", err, raw)
	}
	keyID := created.Key.Metadata.ID
	for name, lane := range lanes {
		t.Run(name, func(t *testing.T) {
			body := `{"metadata":{"name":"lanes-key-` + strings.ReplaceAll(name, " ", "-") + `"},"spec":{` + principal + `,"accountId":"` + lane.id + `"}}`
			code, raw := st.adminDo(http.MethodPost, "/api/keys", body)
			if want := lane.want; (want == http.StatusOK && code != http.StatusCreated) || (want != http.StatusOK && code != want) {
				t.Errorf("create in %s = %d, want %d: %s", name, code, want, raw)
			}
			code, raw = st.adminDo(http.MethodPut, "/api/keys/by-id/"+keyID, `{"metadata":{"name":"lanes-key"},"spec":{`+principal+`,"accountId":"`+lane.id+`"}}`)
			if code != lane.want {
				t.Errorf("update into %s = %d, want %d: %s", name, code, lane.want, raw)
			}
			if lane.want == http.StatusForbidden && !strings.Contains(string(raw), "account_not_held") {
				t.Errorf("update into %s answered %s, want account_not_held", name, raw)
			}
		})
	}
}

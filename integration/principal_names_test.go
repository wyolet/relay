//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// A non-admin who may not list users still reads the names of the holders and principals on the accounts and keys they see: a user by username, a service account by display name.
func TestHolderAndPrincipalNamesForANonAdmin(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	ctx := context.Background()
	teamID := st.mkTeam(t, "names")
	projectID := st.mkProject(t, "names-web", teamID)
	code, raw := st.adminDo(http.MethodPost, "/api/service-accounts",
		`{"metadata":{"name":"names-ci","displayName":"CI deployer"},"spec":{"projectId":"`+projectID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/service-accounts = %d: %s", code, raw)
	}
	var sa tenancyRow
	if err := json.Unmarshal(raw, &sa); err != nil {
		t.Fatal(err)
	}
	leadID := st.seedLogin(t, "names-lead", "pw-lead")
	devID := st.seedLogin(t, "names-dev", "pw-dev")
	st.bindRole(t, "names-lead-admin", roles["team-admin"].Meta.ID, leadID, "team", teamID)
	st.bindRole(t, "names-dev", roles["developer"].Meta.ID, devID, "project", projectID)
	if err := st.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	devAccount := st.accountAt(t, "member:project:"+projectID+":"+devID)
	saAccount := st.accountAt(t, "service:"+sa.Metadata.ID)

	lead := st.login(t, "names-lead", "pw-lead")
	if code, raw := lead.do(http.MethodGet, "/api/users", ""); code != http.StatusForbidden {
		t.Fatalf("the team admin lists users = %d, want 403 (the case this covers): %s", code, raw)
	}

	type principal struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var accounts struct {
		Items []struct {
			Metadata struct {
				ID string `json:"id"`
			} `json:"metadata"`
			Spec struct {
				Holder *principal `json:"holder"`
			} `json:"spec"`
		} `json:"items"`
	}
	code, raw = lead.do(http.MethodGet, "/api/accounts", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/accounts = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &accounts); err != nil {
		t.Fatal(err)
	}
	holders := map[string]*principal{}
	for _, a := range accounts.Items {
		holders[a.Metadata.ID] = a.Spec.Holder
	}
	if h := holders[devAccount]; h == nil || h.ID != devID || h.Name != "names-dev" {
		t.Errorf("the developer's member account holder = %+v, want names-dev", h)
	}
	if h := holders[saAccount]; h == nil || h.ID != sa.Metadata.ID || h.Name != "CI deployer" {
		t.Errorf("the service account's account holder = %+v, want CI deployer", h)
	}

	var one struct {
		Spec struct {
			Holder *principal `json:"holder"`
		} `json:"spec"`
	}
	code, raw = lead.do(http.MethodGet, "/api/accounts/"+devAccount, "")
	if code != http.StatusOK {
		t.Fatalf("GET one account = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &one); err != nil || one.Spec.Holder == nil || one.Spec.Holder.Name != "names-dev" {
		t.Errorf("one account's holder = %+v (%v), want names-dev", one.Spec.Holder, err)
	}

	code, raw = lead.do(http.MethodPost, "/api/keys", `{"metadata":{"name":"names-ci-key"},"spec":{"principal":{"kind":"serviceaccount","id":"`+sa.Metadata.ID+`","name":"ignored"}}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}
	var created struct {
		Key struct {
			Spec struct {
				Principal principal `json:"principal"`
			} `json:"spec"`
		} `json:"key"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.Key.Spec.Principal.Name != "CI deployer" {
		t.Errorf("created key principal = %+v (%v), want CI deployer", created.Key.Spec.Principal, err)
	}
	var keys struct {
		Items []struct {
			Spec struct {
				Principal principal `json:"principal"`
			} `json:"spec"`
		} `json:"items"`
	}
	code, raw = lead.do(http.MethodGet, "/api/keys", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/keys = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &keys); err != nil || len(keys.Items) != 1 || keys.Items[0].Spec.Principal.Name != "CI deployer" {
		t.Errorf("listed keys = %+v (%v), want the service account's key named CI deployer", keys.Items, err)
	}
}

//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/audit"
)

// A SCIM client cannot disable or delete the last enabled admin: the request is refused with a SCIM error, the account stays, and the refusal is an audit row naming the client.
func TestIntegration_SCIMCannotRemoveTheLastAdmin(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`)
	if code != http.StatusCreated {
		t.Fatalf("create SCIM client = %d: %s", code, raw)
	}
	var client struct{ Token string }
	if err := json.Unmarshal(raw, &client); err != nil {
		t.Fatal(err)
	}
	st.enableSCIM()
	created := st.scimWant(client.Token, http.MethodPost, "/Users", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"], "userName": "root@example.com", "externalId": "00u-root", "active": true
	}`, http.StatusCreated)
	id, _ := created["id"].(string)
	if code, raw := st.adminDo(http.MethodPut, "/api/users/by-id/"+id, `{"roles":["admin"]}`); code != http.StatusOK {
		t.Fatalf("grant admin = %d: %s", code, raw)
	}

	deactivate := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`
	st.scimWant(client.Token, http.MethodPatch, "/Users/"+id, deactivate, http.StatusConflict)
	st.scimWant(client.Token, http.MethodDelete, "/Users/"+id, "", http.StatusConflict)
	if u, err := st.users.Get(t.Context(), id); err != nil || u == nil || u.Disabled {
		t.Fatalf("the last admin after refused SCIM writes: %+v, %v", u, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		denied := map[string]bool{}
		for _, ev := range st.auditList("?limit=1000").Events {
			if ev.Actor.Kind == audit.ActorSCIMClient && ev.Actor.Name == "okta" && ev.Outcome.Status == audit.StatusDenied && ev.Resource.ID == id {
				denied[ev.Action] = true
			}
		}
		if denied["users.update"] && denied["users.delete"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("denied audit rows = %v, want the refused disable and delete", denied)
		}
		time.Sleep(100 * time.Millisecond)
	}

	other := st.seedLogin(t, "second-admin", "pw")
	if code, raw := st.adminDo(http.MethodPut, "/api/users/by-id/"+other, `{"roles":["admin"]}`); code != http.StatusOK {
		t.Fatalf("grant a second admin = %d: %s", code, raw)
	}
	st.scimWant(client.Token, http.MethodPatch, "/Users/"+id, deactivate, http.StatusOK)
}

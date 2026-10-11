//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
)

// Rotating a SCIM client's token kills the old token at once and keeps the client's name, so the users and groups it provisioned stay its own under the new token. Only an admin rotates, a revoked client is never rotated back to life, and the rotation is audited.
func TestIntegration_SCIMClientTokenRotates(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	rotator := &role.Role{Meta: meta.Metadata{ID: meta.NewID(), Name: "scim-rotator", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"scim-clients"}, Verbs: []string{"rotate"}}}}}
	if err := st.stores.Role.Upsert(ctx, rotator); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	leadID := st.seedLogin(t, "lead", "pw-lead")
	st.bindRole(t, "scim-rotators", rotator.Meta.ID, leadID, "system", "")
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`)
	var created struct{ Token string }
	if code != http.StatusCreated || json.Unmarshal(raw, &created) != nil || created.Token == "" {
		t.Fatalf("create SCIM client = %d: %s", code, raw)
	}
	st.enableSCIM()
	user := st.scimWant(created.Token, http.MethodPost, "/Users",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"ada@example.com","externalId":"00u-ada","active":true}`, http.StatusCreated)
	userID, _ := user["id"].(string)
	group := st.scimWant(created.Token, http.MethodPost, "/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Engineering","members":[{"value":"`+userID+`"}]}`, http.StatusCreated)
	groupID, _ := group["id"].(string)

	if code, raw := st.login(t, "lead", "pw-lead").do(http.MethodPost, "/api/scim-clients/okta/rotate", ""); code != http.StatusForbidden {
		t.Fatalf("rotate as a non-admin = %d, want 403: %s", code, raw)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/scim-clients/okta/rotate", "")
	var rotated struct {
		Name, Token string
		Revoked     bool
	}
	if code != http.StatusOK || json.Unmarshal(raw, &rotated) != nil || rotated.Token == "" || rotated.Token == created.Token ||
		rotated.Name != "okta" || rotated.Revoked {
		t.Fatalf("rotate = %d: %s", code, raw)
	}

	st.scimWant(created.Token, http.MethodGet, "/Users/"+userID, "", http.StatusUnauthorized)
	if got := st.scimWant(rotated.Token, http.MethodGet, "/Users/"+userID, "", http.StatusOK); got["userName"] != "ada@example.com" {
		t.Fatalf("the provisioned user under the new token: %v", got)
	}
	st.scimWant(rotated.Token, http.MethodGet, "/Groups/"+groupID, "", http.StatusOK)
	st.scimWant(rotated.Token, http.MethodPatch, "/Users/"+userID,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`, http.StatusOK)

	deadline := time.Now().Add(15 * time.Second)
	for {
		var audited bool
		for _, ev := range st.auditList("?limit=1000").Events {
			if ev.Action == "scim-clients.rotate" && ev.Outcome.Status == audit.StatusAllowed && ev.Resource.Name == "okta" {
				audited = true
			}
		}
		if audited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotation has no audit row")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients/okta/revoke", ""); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients/okta/rotate", ""); code != http.StatusConflict {
		t.Fatalf("rotate a revoked client = %d, want 409: %s", code, raw)
	}
	st.scimWant(rotated.Token, http.MethodGet, "/Users/"+userID, "", http.StatusUnauthorized)
	if code, _ := st.adminDo(http.MethodPost, "/api/scim-clients/nobody/rotate", ""); code != http.StatusNotFound {
		t.Fatalf("rotate a missing client = %d, want 404", code)
	}
}

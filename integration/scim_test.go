//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
)

// scimDo sends a SCIM request with the client's bearer token.
func (s *stack) scimDo(token, method, path, body string) (int, map[string]any) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.control.URL+"/scim/v2"+path, bytes.NewReader([]byte(body)))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/scim+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			s.t.Fatalf("%s %s = %d, not JSON: %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func (s *stack) scimWant(token, method, path, body string, want int) map[string]any {
	s.t.Helper()
	code, out := s.scimDo(token, method, path, body)
	if code != want {
		s.t.Fatalf("%s %s = %d, want %d: %v", method, path, code, want, out)
	}
	return out
}

func totalResults(m map[string]any) int { n, _ := m["totalResults"].(float64); return int(n) }

// enableSCIM turns auth:scim on and waits for the setting to reach the catalog.
func (s *stack) enableSCIM() {
	s.t.Helper()
	if code, raw := s.adminDo(http.MethodPut, "/api/settings/auth:scim", `{"enabled":true}`); code != http.StatusOK {
		s.t.Fatalf("enable auth:scim = %d: %s", code, raw)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !settings.AuthSCIMFrom(s.cat).Enabled {
		if time.Now().After(deadline) {
			s.t.Fatal("auth:scim never reached the catalog")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An identity provider provisions a user through SCIM the way Microsoft Entra ID does, and its deactivation reaches the user's live session and tokens.
func TestSCIMEntraProvisioningSequence(t *testing.T) {
	t.Parallel()
	f := newTokenFixture(t)
	ctx := t.Context()

	code, raw := f.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"entra"}`)
	if code != http.StatusCreated {
		t.Fatalf("create SCIM client = %d: %s", code, raw)
	}
	var client struct{ Token string }
	if err := json.Unmarshal(raw, &client); err != nil || client.Token == "" {
		t.Fatalf("create SCIM client: %v %s", err, raw)
	}
	tok := client.Token

	// Off by default: the endpoint answers 404 until auth:scim is enabled.
	f.scimWant(tok, http.MethodGet, "/ServiceProviderConfig", "", http.StatusNotFound)
	f.enableSCIM()
	f.scimWant(tok, http.MethodGet, "/ServiceProviderConfig", "", http.StatusOK)
	if code, _ := f.scimDo("scim_wrong", http.MethodGet, "/Users", ""); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token = %d, want 401", code)
	}

	// Create the user, after the lookup Entra makes first.
	const userName = "ada@contoso.example"
	q := "/Users?filter=" + url.QueryEscape(`userName eq "`+userName+`"`)
	if got := f.scimWant(tok, http.MethodGet, q, "", http.StatusOK); totalResults(got) != 0 {
		t.Fatalf("lookup before create: %v", got)
	}
	created := f.scimWant(tok, http.MethodPost, "/Users", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
		"externalId": "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef",
		"userName": "`+userName+`",
		"active": true,
		"emails": [{"primary": true, "type": "work", "value": "`+userName+`"}],
		"meta": {"resourceType": "User"},
		"name": {"formatted": "Ada Lovelace", "familyName": "Lovelace", "givenName": "Ada"},
		"roles": []
	}`, http.StatusCreated)
	userID, _ := created["id"].(string)

	// Create the group and add the member.
	group := f.scimWant(tok, http.MethodPost, "/Groups", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group", "http://schemas.microsoft.com/2006/11/ResourceManagement/ADSCIM/2.0/Group"],
		"externalId": "8aa1a0c0-c4c3-4bc0-b4a5-2ef676900159",
		"displayName": "Engineering",
		"meta": {"resourceType": "Group"}
	}`, http.StatusCreated)
	groupID, _ := group["id"].(string)
	f.scimWant(tok, http.MethodPatch, "/Groups/"+groupID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "Add", "path": "members", "value": [{"$ref": null, "value": "`+userID+`"}]}]
	}`, http.StatusNoContent)
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return slices.Contains(s.GroupsForUser(userID), "engineering") },
		"the SCIM membership never reached the snapshot")

	// The provisioned account signs in (a password stands in for the identity provider's login) and mints a token.
	hash, err := bcrypt.GenerateFromPassword([]byte("pw-ada"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.stores.Directory.Users.Get(ctx, userID)
	if err != nil || u == nil {
		t.Fatalf("provisioned user: %v, %v", u, err)
	}
	u.PasswordHash = string(hash)
	if err := f.stores.Directory.Users.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	// Setting a password bumps the token version; mint only once the data plane knows the new one.
	withPassword, err := f.stores.Directory.Users.Get(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cat.ReloadTokenVersions(ctx); err != nil {
		t.Fatal(err)
	}
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool {
		v, ok := s.TokenVersion(userID)
		return ok && v == withPassword.TokenVersion
	}, "the password's token version never reached the snapshot")
	us := f.login(t, userName, "pw-ada")
	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	if got, reason := f.chat(token, "test-model"); got != http.StatusOK {
		t.Fatalf("chat before deactivation = %d (%s)", got, reason)
	}

	// Deactivate, as Entra sends it without its compliance flag.
	f.scimWant(tok, http.MethodPatch, "/Users/"+userID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "Replace", "path": "active", "value": "False"}]
	}`, http.StatusOK)
	if code, raw := us.do(http.MethodGet, "/api/auth/whoami", ""); code != http.StatusUnauthorized {
		t.Fatalf("whoami after deactivation = %d, want 401: %s", code, raw)
	}
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return !s.UserEnabled(userID) },
		"the deactivation never reached the snapshot")
	if got, reason := f.chat(token, "test-model"); got != http.StatusUnauthorized || reason != "token_revoked" {
		t.Fatalf("chat after deactivation = %d (%s), want 401 token_revoked", got, reason)
	}

	// Reactivate: the account is back, the token minted before stays dead.
	f.scimWant(tok, http.MethodPatch, "/Users/"+userID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "replace", "path": "active", "value": true}]
	}`, http.StatusOK)
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return s.UserEnabled(userID) },
		"the reactivation never reached the snapshot")
	if got, _ := f.chat(token, "test-model"); got != http.StatusUnauthorized {
		t.Fatalf("a pre-deactivation token after reactivation = %d, want 401", got)
	}
	f.login(t, userName, "pw-ada")

	// Remove the member, then delete the user.
	f.scimWant(tok, http.MethodPatch, "/Groups/"+groupID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "remove", "path": "members[value eq \"`+userID+`\"]"}]
	}`, http.StatusNoContent)
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool { return len(s.GroupsForUser(userID)) == 0 },
		"the member removal never reached the snapshot")
	f.scimWant(tok, http.MethodDelete, "/Users/"+userID, "", http.StatusNoContent)
	f.scimWant(tok, http.MethodGet, "/Users/"+userID, "", http.StatusNotFound)
	if u, err := f.users.Get(ctx, userID); err != nil || u != nil {
		t.Fatalf("user after DELETE: %v, %v", u, err)
	}

	// Every write is in the audit log under the SCIM client.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var actions []string
		for _, ev := range f.auditList("?actor_name=entra&limit=100").Events {
			if ev.Actor.Kind == audit.ActorSCIMClient {
				actions = append(actions, ev.Action)
			}
		}
		if len(actions) >= 7 {
			for _, want := range []string{"users.create", "groups.create", "groups.update", "users.update", "users.delete"} {
				if !slices.Contains(actions, want) {
					t.Fatalf("audit actions %v lack %s", actions, want)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SCIM audit rows after 15s: %v", actions)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A SCIM client may not reach a user an operator created, and a revoked token stops working.
func TestSCIMClientCannotTouchALocalUserAndRevokes(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	local := st.seedLogin(t, "local-op", "pw-local")
	code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`)
	if code != http.StatusCreated {
		t.Fatalf("create SCIM client = %d: %s", code, raw)
	}
	var client struct{ Token string }
	_ = json.Unmarshal(raw, &client)
	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`); code != http.StatusConflict {
		t.Fatalf("second create = %d: %s", code, raw)
	}
	st.enableSCIM()
	st.scimWant(client.Token, http.MethodPatch, "/Users/"+local, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"replace","value":{"active":false}}]}`, http.StatusNotFound)
	st.scimWant(client.Token, http.MethodDelete, "/Users/"+local, "", http.StatusNotFound)
	if u, _ := st.users.Get(t.Context(), local); u == nil || u.Disabled {
		t.Fatalf("local user after the attempts: %+v", u)
	}
	if got := st.scimWant(client.Token, http.MethodGet, "/Users", "", http.StatusOK); totalResults(got) != 0 {
		t.Fatalf("a local user is visible: %v", got)
	}

	code, raw = st.adminDo(http.MethodGet, "/api/scim-clients", "")
	if code != http.StatusOK || !bytes.Contains(raw, []byte(`"name":"okta"`)) || bytes.Contains(raw, []byte(client.Token)) {
		t.Fatalf("list = %d: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients/okta/revoke", ""); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", code, raw)
	}
	st.scimWant(client.Token, http.MethodGet, "/Users", "", http.StatusUnauthorized)
	if code, _ := st.adminDo(http.MethodPost, "/api/scim-clients/nobody/revoke", ""); code != http.StatusNotFound {
		t.Fatalf("revoke a missing client = %d, want 404", code)
	}
}

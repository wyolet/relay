//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The personal-account switch reaches the request path through NOTIFY alone: the deployment setting closes a user's personal lane, the user's own value opens it again, and the users list says which decided.
func TestIntegration_PersonalAccountSwitch(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, tokenUpstreamResponse)
	}))
	t.Cleanup(upstream.Close)
	st := newStack(t)
	bearer := st.seedHappyPath(upstream.URL, "sk-mock-upstream-key")

	chat := func() (int, string) {
		req, err := http.NewRequest(http.MethodPost, st.inference.URL+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		return resp.StatusCode, env.Error.Code
	}
	waitFor := func(what string, wantStatus int, wantCode string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			status, code := chat()
			if status == wantStatus && code == wantCode {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: chat = %d %q, want %d %q", what, status, code, wantStatus, wantCode)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	type userRow struct {
		ID                       string `json:"id"`
		Username                 string `json:"username"`
		PersonalAccount          string `json:"personalAccount"`
		PersonalAccountEffective *struct {
			State  string `json:"state"`
			Source string `json:"source"`
		} `json:"personalAccountEffective"`
	}
	owner := func() userRow {
		t.Helper()
		code, raw := st.adminDo(http.MethodGet, "/api/users", "")
		if code != http.StatusOK {
			t.Fatalf("GET /api/users = %d %s", code, raw)
		}
		var out struct {
			Items []userRow `json:"items"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		for _, u := range out.Items {
			if u.Username == "e2e-owner" {
				return u
			}
		}
		t.Fatalf("no e2e-owner in %s", raw)
		return userRow{}
	}
	waitFor("before", http.StatusOK, "")

	if code, raw := st.adminDo(http.MethodPut, "/api/settings/accounts", `{"personalAccounts":"off"}`); code != http.StatusOK {
		t.Fatalf("PUT /api/settings/accounts = %d %s", code, raw)
	}
	waitFor("setting off", http.StatusForbidden, "personal_account_off")
	if u := owner(); u.PersonalAccount != "" || u.PersonalAccountEffective == nil || u.PersonalAccountEffective.State != "off" || u.PersonalAccountEffective.Source != "setting" {
		t.Fatalf("owner after the setting = %+v, want off from the setting", u)
	}

	id := owner().ID
	put := func(body string) userRow {
		t.Helper()
		code, raw := st.adminDo(http.MethodPut, "/api/users/by-id/"+id, body)
		if code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", body, code, raw)
		}
		var u userRow
		if err := json.Unmarshal(raw, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	// The answer shows the value just saved, before the plans rebuild.
	if u := put(`{"personalAccount":"on"}`); u.PersonalAccountEffective == nil || u.PersonalAccountEffective.State != "on" || u.PersonalAccountEffective.Source != "user" {
		t.Fatalf("PUT answer = %+v, want on from the user", u)
	}
	waitFor("user on", http.StatusOK, "")
	if u := owner(); u.PersonalAccount != "on" || u.PersonalAccountEffective == nil || u.PersonalAccountEffective.State != "on" || u.PersonalAccountEffective.Source != "user" {
		t.Fatalf("owner after their own on = %+v, want on from the user", u)
	}

	// A group's value is stored beside its spec and reaches the plans the same way.
	if code, raw := st.adminDo(http.MethodPut, "/api/settings/accounts", `{"personalAccounts":"on"}`); code != http.StatusOK {
		t.Fatalf("PUT /api/settings/accounts = %d %s", code, raw)
	}
	// Roles and personalAccount in one request commit together.
	if u := put(`{"personalAccount":"","roles":["admin"]}`); u.PersonalAccount != "" {
		t.Fatalf("PUT answer = %+v, want personalAccount cleared", u)
	}
	if u := owner(); u.PersonalAccount != "" {
		t.Fatalf("owner after the combined PUT = %+v, want personalAccount cleared", u)
	}
	code, raw := st.adminDo(http.MethodPost, "/api/groups", `{"metadata":{"name":"no-personal"},"spec":{"memberIds":["`+id+`"],"personalAccount":"off"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/groups = %d %s", code, raw)
	}
	var g struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
		Spec struct {
			PersonalAccount string `json:"personalAccount"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || g.Spec.PersonalAccount != "off" {
		t.Fatalf("created group = %s (%v), want personalAccount off", raw, err)
	}
	waitFor("group off", http.StatusForbidden, "personal_account_off")
	if code, raw := st.adminDo(http.MethodGet, "/api/groups/"+g.Metadata.ID, ""); code != http.StatusOK || !bytes.Contains(raw, []byte(`"personalAccount":"off"`)) {
		t.Fatalf("GET group = %d %s, want personalAccount off read back", code, raw)
	}
	if u := owner(); u.PersonalAccountEffective == nil || u.PersonalAccountEffective.Source != "group" {
		t.Fatalf("owner under the group = %+v, want off from the group", u)
	}
	// A group carrying a value stays on the system: moving it to a user is refused.
	if code, raw := st.adminDo(http.MethodPut, "/api/groups/by-id/"+g.Metadata.ID, `{"metadata":{"name":"no-personal","owner":{"kind":"user","id":"`+id+`"}},"spec":{"memberIds":["`+id+`"],"personalAccount":"off"}}`); code != http.StatusBadRequest {
		t.Fatalf("moving the group to a user = %d %s, want 400", code, raw)
	}
}

// personalAccount is relay's own attribute, so an admin sets it on a group a SCIM client manages; the group's members stay the client's.
func TestIntegration_PersonalAccountOnSCIMGroup(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"entra"}`)
	if code != http.StatusCreated {
		t.Fatalf("create SCIM client = %d: %s", code, raw)
	}
	var client struct{ Token string }
	if err := json.Unmarshal(raw, &client); err != nil || client.Token == "" {
		t.Fatalf("create SCIM client: %v %s", err, raw)
	}
	st.enableSCIM()
	created := st.scimWant(client.Token, http.MethodPost, "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"externalId":"g-ext-1","displayName":"Pilots"}`, http.StatusCreated)
	id, _ := created["id"].(string)

	read := func() map[string]any {
		t.Helper()
		code, raw := st.adminDo(http.MethodGet, "/api/groups/"+id, "")
		if code != http.StatusOK {
			t.Fatalf("GET group = %d %s", code, raw)
		}
		var g map[string]any
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		delete(g, "$schema")
		return g
	}
	g := read()
	spec, _ := g["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		g["spec"] = spec
	}
	spec["personalAccount"] = "off"
	body, _ := json.Marshal(g)
	if code, raw := st.adminDo(http.MethodPut, "/api/groups/by-id/"+id, string(body)); code != http.StatusOK {
		t.Fatalf("admin setting personalAccount on a SCIM group = %d %s, want 200", code, raw)
	}
	after := read()
	if s, _ := after["spec"].(map[string]any); s["personalAccount"] != "off" || after["external"] == nil {
		t.Fatalf("group after = %v, want personalAccount off and still managed", after)
	}

	// Its members stay the source's.
	after["spec"].(map[string]any)["memberIds"] = []string{st.seedUser("not-from-scim")}
	body, _ = json.Marshal(after)
	if code, raw := st.adminDo(http.MethodPut, "/api/groups/by-id/"+id, string(body)); code != http.StatusConflict || !bytes.Contains(raw, []byte("managed_by_source")) {
		t.Fatalf("admin changing a SCIM group's members = %d %s, want 409 managed_by_source", code, raw)
	}
}

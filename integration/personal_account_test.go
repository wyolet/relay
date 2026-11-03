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
	if code, raw := st.adminDo(http.MethodPut, "/api/users/by-id/"+id, `{"personalAccount":"on"}`); code != http.StatusOK {
		t.Fatalf("PUT personalAccount = %d %s", code, raw)
	}
	waitFor("user on", http.StatusOK, "")
	if u := owner(); u.PersonalAccount != "on" || u.PersonalAccountEffective == nil || u.PersonalAccountEffective.State != "on" || u.PersonalAccountEffective.Source != "user" {
		t.Fatalf("owner after their own on = %+v, want on from the user", u)
	}

	// A group's value is stored beside its spec and reaches the plans the same way.
	if code, raw := st.adminDo(http.MethodPut, "/api/settings/accounts", `{"personalAccounts":"on"}`); code != http.StatusOK {
		t.Fatalf("PUT /api/settings/accounts = %d %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPut, "/api/users/by-id/"+id, `{"personalAccount":""}`); code != http.StatusOK {
		t.Fatalf("clear personalAccount = %d %s", code, raw)
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
}

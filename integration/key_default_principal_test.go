//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A key created without a principal is the signed-in user's, in their personal account; the admin token is no user and must name one.
func TestKeyWithoutPrincipalIsTheCallers(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	userID := st.seedLogin(t, "keyless", "pw-keyless")
	personal := st.accountAt(t, "personal:"+userID)
	us := st.login(t, "keyless", "pw-keyless")

	code, raw := us.do(http.MethodPost, "/api/keys", `{"metadata":{"name":"mine"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/keys without a principal = %d: %s", code, raw)
	}
	var created struct {
		Key struct {
			Metadata struct {
				Owner struct {
					Kind string `json:"kind"`
					ID   string `json:"id"`
				} `json:"owner"`
			} `json:"metadata"`
			Spec struct {
				Principal struct {
					Kind string `json:"kind"`
					ID   string `json:"id"`
				} `json:"principal"`
				AccountID string `json:"accountId"`
			} `json:"spec"`
		} `json:"key"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	k := created.Key
	if k.Spec.Principal.Kind != "user" || k.Spec.Principal.ID != userID || k.Metadata.Owner.Kind != "user" || k.Metadata.Owner.ID != userID || k.Spec.AccountID != personal {
		t.Errorf("key = %+v, want the caller's own key in their personal account %s", k, personal)
	}

	code, raw = st.adminDo(http.MethodPost, "/api/keys", `{"metadata":{"name":"nobodys"},"spec":{}}`)
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "spec.principal is required") {
		t.Errorf("admin token without a principal = %d, want 400 asking for one: %s", code, raw)
	}
}

//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/user"
)

// A login on a browser that still holds an admin's session must not hand the next account that admin's role.
func TestIntegration_LoginOverAnotherSessionInheritsNothing(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	aliceID := st.seedLogin(t, "alice", "pw-alice")
	alice, _ := st.users.Get(t.Context(), aliceID)
	alice.Roles = []string{user.RoleAdmin}
	if err := st.users.Upsert(t.Context(), alice); err != nil {
		t.Fatal(err)
	}
	bobID := st.seedLogin(t, "bob", "pw-bob")

	browser := st.login(t, "alice", "pw-alice")
	if code, raw := browser.do(http.MethodPost, "/api/auth/login", `{"username":"bob","password":"pw-bob"}`); code != http.StatusOK {
		t.Fatalf("bob's login over alice's session = %d: %s", code, raw)
	}
	code, raw := browser.do(http.MethodGet, "/api/auth/whoami", "")
	if code != http.StatusOK {
		t.Fatalf("whoami = %d: %s", code, raw)
	}
	var who struct {
		UserID string   `json:"user_id"`
		Roles  []string `json:"roles"`
	}
	if err := json.Unmarshal(raw, &who); err != nil {
		t.Fatalf("decode whoami: %v (%s)", err, raw)
	}
	if who.UserID != bobID || slices.Contains(who.Roles, user.RoleAdmin) {
		t.Fatalf("after bob's login: user %s roles %v, want bob with no admin role", who.UserID, who.Roles)
	}
}

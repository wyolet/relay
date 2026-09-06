//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// An upgraded deployment has no bindings yet: every scoped list must answer
// an empty 200 rather than 403, and a project-bound developer must be able
// to read the team their project belongs to.
func TestIntegration_ScopedListsAndParentTeamReads(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamA)
	st.seedLogin(t, "nobody", "pw-nobody")
	devID := st.seedLogin(t, "dev", "pw-dev")
	st.bindRole(t, "alpha-devs", roles["developer"].Meta.ID, devID, "project", projA)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	nobody := st.login(t, "nobody", "pw-nobody")
	for _, path := range []string{"/api/keys", "/api/policies", "/api/projects", "/api/teams", "/api/service-accounts"} {
		code, raw := nobody.do(http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s with no bindings = %d, want 200: %s", path, code, raw)
		}
		var list struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if list.Total != 0 {
			t.Errorf("GET %s returned %d rows to a caller with no bindings", path, list.Total)
		}
	}
	// users stays global-only.
	if code, raw := nobody.do(http.MethodGet, "/api/users", ""); code != http.StatusForbidden {
		t.Fatalf("users list without a global binding = %d, want 403: %s", code, raw)
	}

	dev := st.login(t, "dev", "pw-dev")
	if code, raw := dev.do(http.MethodGet, "/api/teams/alpha", ""); code != http.StatusOK {
		t.Fatalf("project-bound developer reading the parent team = %d, want 200: %s", code, raw)
	}
}

// A developer's personal key may name their project's policy: they may
// create keys there, and the key then carries the project. Someone outside
// the project is still refused.
func TestIntegration_PersonalKeyOnAProjectPolicy(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamA)
	devID := st.seedLogin(t, "dev", "pw-dev")
	outsiderID := st.seedLogin(t, "outsider", "pw-outsider")
	st.bindRole(t, "alpha-devs", roles["developer"].Meta.ID, devID, "project", projA)

	code, raw := st.adminDo(http.MethodPost, "/api/policies",
		`{"metadata":{"name":"alpha-pol","owner":{"kind":"project","id":"`+projA+`"}},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}
	var pol tenancyRow
	if err := json.Unmarshal(raw, &pol); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	dev := st.login(t, "dev", "pw-dev")
	body := func(name, userID string) string {
		return `{"metadata":{"name":"` + name + `"},` +
			`"spec":{"principal":{"kind":"user","id":"` + userID + `"},"policyId":"` + pol.Metadata.ID + `"}}`
	}
	if code, raw := dev.do(http.MethodPost, "/api/keys", body("dev-personal", devID)); code != http.StatusCreated {
		t.Fatalf("developer's personal key on their project's policy = %d, want 201: %s", code, raw)
	}

	outsider := st.login(t, "outsider", "pw-outsider")
	if code, raw := outsider.do(http.MethodPost, "/api/keys", body("outsider-personal", outsiderID)); code == http.StatusCreated {
		t.Fatalf("a personal key outside the project was accepted: %s", raw)
	}
}

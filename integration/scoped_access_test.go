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
	t.Parallel()
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

// A project admin's pool serves the project's policies; a personal policy
// of someone outside the project cannot draw on it.
func TestIntegration_ProjectPoolOnlyForTheProject(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamA)
	adminID := st.seedLogin(t, "padmin", "pw-padmin")
	st.seedLogin(t, "outsider", "pw-outsider")
	st.bindRole(t, "alpha-admins", roles["project-admin"].Meta.ID, adminID, "project", projA)
	hostID, _, _ := seedHostTiers(t, st)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	padmin := st.login(t, "padmin", "pw-padmin")
	code, raw := padmin.do(http.MethodPost, "/api/pools",
		`{"metadata":{"name":"alpha-pool","owner":{"kind":"project","id":"`+projA+`"}},"spec":{"hostId":"`+hostID+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/pools = %d: %s", code, raw)
	}
	var pl tenancyRow
	if err := json.Unmarshal(raw, &pl); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	body := func(name, owner string) string {
		return `{"metadata":{"name":"` + name + `"` + owner + `},"spec":{"poolIds":["` + pl.Metadata.ID + `"]}}`
	}
	if code, raw := padmin.do(http.MethodPost, "/api/policies", body("alpha-pol", `,"owner":{"kind":"project","id":"`+projA+`"}`)); code != http.StatusCreated {
		t.Fatalf("project policy on the project's pool = %d, want 201: %s", code, raw)
	}

	outsider := st.login(t, "outsider", "pw-outsider")
	if code, raw := outsider.do(http.MethodPost, "/api/policies", body("stolen-pool", "")); code == http.StatusCreated {
		t.Fatalf("a personal policy outside the project drew on its pool: %s", raw)
	}
}

// Every catalog tier rate limit is host-owned; a project admin must be able
// to put one on the project's own policy.
func TestIntegration_ProjectPolicyOnACatalogRateLimit(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamA)
	adminID := st.seedLogin(t, "padmin", "pw-padmin")
	st.bindRole(t, "alpha-admins", roles["project-admin"].Meta.ID, adminID, "project", projA)
	_, rateLimitID, _ := seedHostTiers(t, st)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	padmin := st.login(t, "padmin", "pw-padmin")
	body := `{"metadata":{"name":"alpha-limited","owner":{"kind":"project","id":"` + projA + `"}},` +
		`"spec":{"rateLimits":[{"rateLimitId":"` + rateLimitID + `"}]}}`
	if code, raw := padmin.do(http.MethodPost, "/api/policies", body); code != http.StatusCreated {
		t.Fatalf("project policy on a host-owned rate limit = %d, want 201: %s", code, raw)
	}
}

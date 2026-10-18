//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// PUT honours metadata.owner: the row moves when the caller may create in
// the new scope and nothing referencing it is left in another scope.
func TestIntegration_UpdateMovesOwner(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamID := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamID)
	projB := st.mkProject(t, "alpha-two", teamID)
	projC := st.mkProject(t, "alpha-three", teamID)
	aliceID := st.seedLogin(t, "alice", "pw-alice")
	st.bindRole(t, "alice-a", roles["project-admin"].Meta.ID, aliceID, "project", projA)
	st.bindRole(t, "alice-b", roles["project-admin"].Meta.ID, aliceID, "project", projB)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	alice := st.login(t, "alice", "pw-alice")

	createPolicy := func(name string) string {
		t.Helper()
		code, raw := alice.do(http.MethodPost, "/api/policies", `{"metadata":{"name":"`+name+`"},"spec":{}}`)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/policies %s = %d: %s", name, code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode policy: %v", err)
		}
		if row.Metadata.Owner.Kind != "user" || row.Metadata.Owner.ID != aliceID {
			t.Fatalf("policy %s owner = %+v, want alice", name, row.Metadata.Owner)
		}
		return row.Metadata.ID
	}
	put := func(as *userSession, id, name, owner string) (int, []byte) {
		body := `{"metadata":{"name":"` + name + `"` + owner + `},"spec":{}}`
		if as == nil {
			return st.adminDo(http.MethodPut, "/api/policies/by-id/"+id, body)
		}
		return as.do(http.MethodPut, "/api/policies/by-id/"+id, body)
	}
	ownerOf := func(id string) (kind, ownerID string) {
		t.Helper()
		code, raw := st.adminDo(http.MethodGet, "/api/policies/"+id, "")
		if code != http.StatusOK {
			t.Fatalf("GET policy = %d: %s", code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode policy: %v", err)
		}
		return row.Metadata.Owner.Kind, row.Metadata.Owner.ID
	}
	projectOwner := func(id string) string { return `,"owner":{"kind":"project","id":"` + id + `"}` }

	pol := createPolicy("moving")
	if code, raw := put(alice, pol, "moving", projectOwner(projA)); code != http.StatusOK {
		t.Fatalf("move to a project alice may create in = %d, want 200: %s", code, raw)
	}
	if kind, id := ownerOf(pol); kind != "project" || id != projA {
		t.Fatalf("owner after move = %s/%s, want project/%s", kind, id, projA)
	}

	stuck := createPolicy("stuck")
	if code, raw := put(alice, stuck, "stuck", projectOwner(projC)); code != http.StatusForbidden {
		t.Fatalf("move to a project alice may not create in = %d, want 403: %s", code, raw)
	}
	if kind, id := ownerOf(stuck); kind != "user" || id != aliceID {
		t.Fatalf("refused move changed the owner to %s/%s", kind, id)
	}

	if code, raw := put(alice, pol, "moving", ""); code != http.StatusOK {
		t.Fatalf("update without an owner = %d: %s", code, raw)
	}
	if kind, id := ownerOf(pol); kind != "project" || id != projA {
		t.Fatalf("omitted owner moved the row to %s/%s", kind, id)
	}

	// A project-A key on the policy keeps it in project A or a shared scope.
	saA := st.mkServiceAccount(t, "alpha-bot", projA)
	if code, raw := st.adminDo(http.MethodPost, "/api/keys",
		`{"metadata":{"name":"alpha-key"},"spec":{"principal":{"kind":"serviceaccount","id":"`+saA+`"},"policyId":"`+pol+`"}}`); code != http.StatusCreated {
		t.Fatalf("POST /api/keys = %d: %s", code, raw)
	}
	code, raw := put(alice, pol, "moving", projectOwner(projB))
	if code != http.StatusConflict || !strings.Contains(string(raw), "resource_in_use") || !strings.Contains(string(raw), "alpha-key") {
		t.Fatalf("move stranding a project key = %d, want 409 naming the key: %s", code, raw)
	}
	if kind, id := ownerOf(pol); kind != "project" || id != projA {
		t.Fatalf("refused move changed the owner to %s/%s", kind, id)
	}

	// The account's key takes its owner from the account's project.
	code, raw = alice.do(http.MethodPut, "/api/service-accounts/by-id/"+saA,
		`{"metadata":{"name":"alpha-bot"`+projectOwner(projB)+`},"spec":{"projectId":"`+projB+`"}}`)
	if code != http.StatusConflict || !strings.Contains(string(raw), "alpha-key") {
		t.Fatalf("moving a service account with a key = %d, want 409 naming the key: %s", code, raw)
	}

	// A project-A policy on a project-A rate limit keeps the limit there.
	code, raw = alice.do(http.MethodPost, "/api/rate-limits",
		`{"metadata":{"name":"alpha-limit"`+projectOwner(projA)+`},"spec":{"rules":[{"meter":"requests","amount":10,"window":60,"strategy":"token-bucket"}]}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/rate-limits = %d: %s", code, raw)
	}
	var rl tenancyRow
	if err := json.Unmarshal(raw, &rl); err != nil {
		t.Fatalf("decode rate limit: %v", err)
	}
	if code, raw := alice.do(http.MethodPost, "/api/policies",
		`{"metadata":{"name":"limited"`+projectOwner(projA)+`},"spec":{"rateLimitId":"`+rl.Metadata.ID+`"}}`); code != http.StatusCreated {
		t.Fatalf("POST /api/policies limited = %d: %s", code, raw)
	}
	code, raw = alice.do(http.MethodPut, "/api/rate-limits/by-id/"+rl.Metadata.ID,
		`{"metadata":{"name":"alpha-limit"`+projectOwner(projB)+`},"spec":{"rules":[{"meter":"requests","amount":10,"window":60,"strategy":"token-bucket"}]}}`)
	if code != http.StatusConflict || !strings.Contains(string(raw), "limited") {
		t.Fatalf("moving a rate limit a project-A policy uses = %d, want 409 naming the policy: %s", code, raw)
	}

	if code, raw := put(nil, pol, "moving", `,"owner":{"kind":"system"}`); code != http.StatusOK {
		t.Fatalf("admin move to system = %d, want 200: %s", code, raw)
	}
	if kind, _ := ownerOf(pol); kind != "system" {
		t.Fatalf("owner after admin move = %s, want system", kind)
	}

	if !auditRecordsOwnerChange(st, pol) {
		t.Fatal("no policies audit row lists metadata.owner for the moved policy")
	}
}

// auditRecordsOwnerChange polls for an audit row on the policy's PUT whose
// change lists metadata.owner; the emitter batches on a 1s tick.
func auditRecordsOwnerChange(st *stack, id string) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range st.auditList("?resource_kind=policy&limit=1000").Events {
			if ev.Request.Path == "/api/policies/by-id/"+id && ev.Change != nil && slices.Contains(ev.Change.Fields, "metadata.owner") {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// Shared catalog kinds stay ownerless: PUT cannot hand one to a tenant.
func TestIntegration_UpdateCannotMoveSharedCatalogRowToTenant(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	teamID := st.mkTeam(t, "alpha")
	projA := st.mkProject(t, "alpha-one", teamID)
	code, raw := st.adminDo(http.MethodPost, "/api/hosts", `{"metadata":{"name":"acme"},"spec":{"baseURL":"https://acme.example.com"}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/hosts = %d: %s", code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode host: %v", err)
	}
	code, raw = st.adminDo(http.MethodPut, "/api/hosts/by-id/"+row.Metadata.ID,
		`{"metadata":{"name":"acme","owner":{"kind":"project","id":"`+projA+`"}},"spec":{"baseURL":"https://acme.example.com"}}`)
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "shared catalog data") {
		t.Fatalf("PUT host to a project owner = %d, want 400: %s", code, raw)
	}
}

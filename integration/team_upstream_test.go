//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

// A team owns host keys and rate limits like its pools: they serve the team and its projects, the team's keys join an account of the team's own, and they keep the team from being deleted.
func TestIntegration_TeamOwnedHostKeysAndRateLimits(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	teamA, teamB := st.mkTeam(t, "upstream-a"), st.mkTeam(t, "upstream-b")
	projA, projB := st.mkProject(t, "upstream-a-one", teamA), st.mkProject(t, "upstream-b-one", teamB)
	hostID, _, _ := seedHostTiers(t, st)
	ownerA := `{"kind":"team","id":"` + teamA + `"}`

	create := func(path, body string) tenancyRow {
		t.Helper()
		code, raw := st.adminDo(http.MethodPost, path, body)
		if code != http.StatusCreated {
			t.Fatalf("POST %s = %d: %s", path, code, raw)
		}
		var row tenancyRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return row
	}
	org := create("/api/accounts", `{"metadata":{"name":"team-a-org","owner":`+ownerA+`},"spec":{"hostId":"`+hostID+`"}}`)
	key := create("/api/host-keys", `{"metadata":{"name":"team-a-key","owner":`+ownerA+`},"spec":{"hostId":"`+hostID+`","accountId":"`+org.Metadata.ID+`","valueFrom":{"kind":"env","env":"TEAM_A_UPSTREAM_KEY"}}}`)
	stored, err := st.stores.HostKey.Get(context.Background(), key.Metadata.ID)
	if err != nil || stored == nil {
		t.Fatalf("get key: %v", err)
	}
	acct, err := st.stores.Account.Get(context.Background(), stored.Spec.AccountID)
	if err != nil || acct == nil || acct.Meta.Owner != (meta.Owner{Kind: meta.OwnerTeam, ID: teamA}) {
		t.Fatalf("team key's account = %+v, %v; want the team's account", acct, err)
	}
	limit := create("/api/rate-limits", `{"metadata":{"name":"team-a-rpm","owner":`+ownerA+`},"spec":{"rules":[{"meter":"requests","amount":60,"window":60,"strategy":"token-bucket"}]}}`)
	pool := create("/api/pools", `{"metadata":{"name":"team-a-pool","owner":`+ownerA+`},"spec":{"hostId":"`+hostID+`","credentialIds":["`+key.Metadata.ID+`"]}}`)

	policy := func(name, projectID string) string {
		return `{"metadata":{"name":"` + name + `","owner":{"kind":"project","id":"` + projectID + `"}},` +
			`"spec":{"poolIds":["` + pool.Metadata.ID + `"],"rateLimits":[{"rateLimitId":"` + limit.Metadata.ID + `"}]}}`
	}
	create("/api/policies", policy("upstream-a-pol", projA))
	if code, raw := st.adminDo(http.MethodPost, "/api/policies", policy("upstream-b-pol", projB)); code != http.StatusBadRequest {
		t.Fatalf("another team's project linked team A's rows = %d, want 400: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/rate-limits", `{"metadata":{"name":"no-team","owner":{"kind":"team"}},"spec":{"rules":[{"meter":"requests","amount":1,"window":60,"strategy":"token-bucket"}]}}`); code != http.StatusBadRequest {
		t.Fatalf("team rate limit without the team's id = %d, want 400: %s", code, raw)
	}

	code, raw := st.adminDo(http.MethodDelete, "/api/teams/by-id/"+teamA, "")
	if code != http.StatusConflict || !strings.Contains(string(raw), `"team-a-key"`) || !strings.Contains(string(raw), `"team-a-rpm"`) {
		t.Fatalf("DELETE a team owning a host key and a rate limit = %d: %s, want 409 naming both", code, raw)
	}
}

//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A manifest apply is a write like any other: it moves the version, so an
// editor holding the row from before the apply gets a 409 instead of
// silently reverting it. A no-op re-apply writes nothing and keeps it.
func TestIntegration_ApplyMovesResourceVersion(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()

	if code, plan, raw := st.applyBundle(bundle, ""); code != http.StatusOK || !plan.Applied {
		t.Fatalf("apply: %d %s", code, raw)
	}
	teams, err := st.stores.Team.List(ctx)
	if err != nil || len(teams) != 1 {
		t.Fatalf("list teams: %v %d", err, len(teams))
	}
	id := teams[0].Meta.ID

	code, opened := st.adminDo(http.MethodGet, "/api/teams/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET team = %d: %s", code, opened)
	}
	var row map[string]any
	if err := json.Unmarshal(opened, &row); err != nil {
		t.Fatalf("decode team: %v", err)
	}
	before, _ := row["metadata"].(map[string]any)["resourceVersion"].(string)
	if before == "" {
		t.Fatalf("GET carries no metadata.resourceVersion: %s", opened)
	}

	if _, plan, _ := st.applyBundle(bundle, ""); plan.Counts.Unchanged != 4 {
		t.Fatalf("re-apply counts = %+v", plan.Counts)
	}
	unchanged, _ := st.stores.Team.Get(ctx, id)
	if unchanged.Meta.ResourceVersion != before {
		t.Fatalf("a no-op apply moved the version %s -> %s", before, unchanged.Meta.ResourceVersion)
	}

	edited := strings.Replace(bundle, "displayName: Platform", "displayName: Platform Engineering", 1)
	if _, plan, _ := st.applyBundle(edited, ""); plan.action("Team", "platform") != "update" {
		t.Fatalf("edited apply plan = %+v", plan.Plan)
	}
	after, _ := st.stores.Team.Get(ctx, id)
	if after.Meta.ResourceVersion == before {
		t.Fatalf("apply update left the version at %s", before)
	}

	row["metadata"].(map[string]any)["displayName"] = "Stale Form"
	body, _ := json.Marshal(row)
	code, raw := st.adminDo(http.MethodPut, "/api/teams/by-id/"+id, string(body))
	if code != http.StatusConflict || !strings.Contains(string(raw), `"stale_resource_version"`) {
		t.Fatalf("stale PUT after apply = %d: %s", code, raw)
	}
	kept, _ := st.stores.Team.Get(ctx, id)
	if kept.Meta.DisplayName != "Platform Engineering" {
		t.Fatalf("stale PUT reverted the applied row: %q", kept.Meta.DisplayName)
	}
}

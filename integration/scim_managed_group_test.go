//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Apply over a group a SCIM client manages is refused while planning, so the rest of the bundle is not written first; the group's read shows its source, and a forced apply writes over it.
func TestIntegration_ApplyOverAManagedGroupWritesNothing(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`)
	if code != http.StatusCreated {
		t.Fatalf("create SCIM client = %d: %s", code, raw)
	}
	var client struct{ Token string }
	if err := json.Unmarshal(raw, &client); err != nil || client.Token == "" {
		t.Fatalf("create SCIM client: %v %s", err, raw)
	}
	st.enableSCIM()
	st.scimWant(client.Token, http.MethodPost, "/Groups", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group"],
		"externalId": "00g-eng", "displayName": "Engineering"
	}`, http.StatusCreated)

	code, raw = st.adminDo(http.MethodGet, "/api/groups/engineering", "")
	var g struct {
		External struct{ Source, ID string } `json:"external"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &g) != nil || g.External.Source != "okta" || g.External.ID != "00g-eng" {
		t.Fatalf("GET the managed group = %d: %s, want its external source and id", code, raw)
	}

	bundle := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: core, owner: {kind: system}}\nspec: {}\n" +
		"---\napiVersion: relay.wyolet.dev/v1alpha2\nkind: Group\nmetadata: {name: engineering, displayName: Eng, owner: {kind: system}}\nspec: {}\n"
	code, _, raw = st.applyBundle(bundle, "")
	if code != http.StatusConflict || !strings.Contains(string(raw), "managed by okta") || strings.Contains(string(raw), `"applied"`) {
		t.Fatalf("apply over a managed group = %d, want a 409 plan refusal: %s", code, raw)
	}
	if teams, err := st.stores.Team.List(t.Context()); err != nil || len(teams) != 0 {
		t.Fatalf("the refused bundle wrote %d teams (%v)", len(teams), err)
	}
	if code, _, raw := st.applyBundle(bundle, "force=true"); code != http.StatusOK {
		t.Fatalf("a forced apply = %d: %s", code, raw)
	}
}

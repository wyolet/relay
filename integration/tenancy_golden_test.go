//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/tenancy_golden.txt from this run")

// tenancyBundle covers every tenancy and RBAC kind apply writes: teams and projects with budgets, a service account with a policy override, a group, a custom role, and bindings at the global, team and project scopes naming users, a group, a service account and every authenticated caller.
const tenancyBundle = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata: {name: platform, displayName: Platform, owner: {kind: user}, labels: {managed: gitops}}
spec: {budget: {amount: "100"}}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata: {name: research, displayName: Research, owner: {kind: user}}
spec: {enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata: {name: ml-search, displayName: ML Search, annotations: {note: seeded}}
spec: {team: platform, budget: {amount: "25.50", period: week}}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata: {name: sandbox, displayName: Sandbox}
spec: {team: research}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: ml-search-default, owner: {kind: project, name: ml-search}}
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: ServiceAccount
metadata: {name: search-indexer, displayName: Search Indexer}
spec: {project: ml-search, policy: ml-search-default}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: ServiceAccount
metadata: {name: sandbox-bot}
spec: {project: sandbox, enabled: false}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Group
metadata: {name: eng, displayName: Engineering}
spec: {members: [alice, bob]}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Role
metadata: {name: key-reader, displayName: Key Reader}
spec:
  rules:
    - {kinds: [keys], verbs: [get, list]}
    - {kinds: [policies], verbs: [get]}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata: {name: platform-admins}
spec:
  role: team-admin
  scope: {kind: team, name: platform}
  subjects: [{kind: group, name: eng}]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata: {name: ml-readers}
spec:
  role: key-reader
  scope: {kind: project, name: ml-search}
  subjects: [{kind: user, name: bob}, {kind: serviceaccount, name: search-indexer}]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata: {name: everyone-reads}
spec:
  role: key-reader
  scope: {kind: system}
  subjects: [{kind: group, name: "system:authenticated"}]
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata: {name: research-auditors}
spec:
  role: auditor
  scope: {kind: team, name: research}
  subjects: [{kind: user, name: carol}]
  enabled: false
`

// TestIntegration_TenancyRowsAndDecisionsMatchGolden applies the bundle, then edits the rows through the control API, and compares every stored row, every response status and every RBAC decision with the golden file. The golden was captured from the code that wrote these tables before the auth module did; regenerate it only from that code, with -update-golden.
func TestIntegration_TenancyRowsAndDecisionsMatchGolden(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	if err := role.SeedBuiltins(ctx, st.stores.Role, nil, nil); err != nil {
		t.Fatalf("seed built-in roles: %v", err)
	}
	for _, u := range []*user.User{
		{ID: ids.New(), Username: "alice", Email: "alice@example.com", OIDCSubject: "https://idp.example.com|alice"},
		{ID: ids.New(), Username: "bob"},
		{ID: ids.New(), Username: "carol"},
		{ID: ids.New(), Username: "dave", Roles: []string{user.RoleAdmin}},
	} {
		if err := st.users.Upsert(ctx, u); err != nil {
			t.Fatalf("user %s: %v", u.Username, err)
		}
	}
	plan, err := apply.Plan(ctx, parseBundle(t, tenancyBundle), apply.Options{Stores: applyStores(st)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := apply.Execute(ctx, plan, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var got []string
	got = append(got, editThroughAPI(t, st)...)
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	names := rowNames(t, st)
	got = append(got, tenancyRows(t, st, names)...)
	got = append(got, rbacDecisions(t, st, names)...)

	path := filepath.Join("testdata", "tenancy_golden.txt")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-golden): %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	diff(t, got, want)
}

// editThroughAPI runs the control API's write paths for every kind the directory now owns and returns one line per call: method, route with ids named, status.
func editThroughAPI(t *testing.T, st *stack) []string {
	t.Helper()
	var out []string
	id := func(plural, name string) string {
		code, body := st.adminDo("GET", "/api/"+plural+"/"+name, "")
		if code != 200 {
			t.Fatalf("GET %s/%s = %d: %s", plural, name, code, body)
		}
		var row struct {
			Metadata struct{ ID string } `json:"metadata"`
		}
		_ = json.Unmarshal(body, &row)
		return row.Metadata.ID
	}
	userID := func(name string) string {
		u, err := st.users.ByUsername(context.Background(), name)
		if err != nil || u == nil {
			t.Fatalf("user %s: %v", name, err)
		}
		return u.ID
	}
	// edit reads the row, applies change to its JSON and writes it back.
	edit := func(plural, name string, change func(row map[string]any)) {
		code, body := st.adminDo("GET", "/api/"+plural+"/"+name, "")
		if code != 200 {
			t.Fatalf("GET %s/%s = %d", plural, name, code)
		}
		var row map[string]any
		_ = json.Unmarshal(body, &row)
		change(row)
		blob, _ := json.Marshal(row)
		rowID := row["metadata"].(map[string]any)["id"].(string)
		code, body = st.adminDo("PUT", "/api/"+plural+"/by-id/"+rowID, string(blob))
		out = append(out, statusLine("PUT", plural+"/"+name, code, body))
	}
	call := func(method, route, label, body string) {
		code, resp := st.adminDo(method, "/api/"+route, body)
		out = append(out, statusLine(method, label, code, resp))
	}
	spec := func(row map[string]any) map[string]any { return row["spec"].(map[string]any) }

	edit("teams", "research", func(r map[string]any) {
		r["metadata"].(map[string]any)["displayName"] = "Research Lab"
		spec(r)["budget"] = map[string]any{"amount": "40", "period": "day"}
	})
	edit("projects", "ml-search", func(r map[string]any) { delete(spec(r), "budget") })
	edit("service-accounts", "sandbox-bot", func(r map[string]any) {
		r["metadata"].(map[string]any)["displayName"] = "Sandbox Bot"
		spec(r)["enabled"] = true
	})
	edit("groups", "eng", func(r map[string]any) { spec(r)["memberIds"] = []string{userID("alice")} })
	edit("role-bindings", "research-auditors", func(r map[string]any) { spec(r)["enabled"] = true })
	edit("roles", "key-reader", func(r map[string]any) { r["metadata"].(map[string]any)["displayName"] = "Readers" })
	call("POST", "groups", "groups", `{"metadata":{"name":"","displayName":"Ops Team"},"spec":{"memberIds":["`+userID("carol")+`"]}}`)
	call("POST", "role-bindings", "role-bindings", `{"metadata":{"name":"ops-auditors"},"spec":{"roleId":"`+id("roles", "auditor")+
		`","scope":{"kind":"project","id":"`+id("projects", "sandbox")+`"},"subjects":[{"kind":"group","name":"ops-team"}]}}`)
	call("POST", "projects", "projects", `{"metadata":{"name":"","displayName":"Scratch Pad"},"spec":{"teamId":"`+id("teams", "research")+`"}}`)
	call("DELETE", "projects/by-id/"+id("projects", "scratch-pad"), "projects/scratch-pad", "")
	call("DELETE", "teams/by-id/"+id("teams", "research"), "teams/research", "")
	call("DELETE", "role-bindings/by-id/"+id("role-bindings", "everyone-reads"), "role-bindings/everyone-reads", "")
	call("POST", "service-accounts", "service-accounts", `{"metadata":{"name":"etl"},"spec":{"projectId":"`+id("projects", "sandbox")+`"}}`)
	call("DELETE", "service-accounts/by-id/"+id("service-accounts", "etl"), "service-accounts/etl", "")
	call("PUT", "users/by-id/"+userID("bob"), "users/bob", `{"disabled":true}`)
	call("PUT", "users/by-id/"+userID("carol"), "users/carol", `{"roles":["auditor"]}`)
	call("PUT", "users/by-id/"+userID("dave"), "users/dave", `{"disabled":true}`)
	call("PUT", "users/by-id/"+userID("alice"), "users/alice", `{"roles":["admin","auditor"]}`)
	call("PUT", "users/by-id/"+userID("dave"), "users/dave", `{"disabled":true}`)
	call("PUT", "users/by-id/"+userID("dave"), "users/dave", `{"disabled":false,"roles":["admin","viewer"]}`)
	call("PUT", "users/by-id/"+userID("carol"), "users/carol", `{"disabled":true}`)
	call("PUT", "users/by-id/"+userID("carol"), "users/carol", `{"disabled":false}`)
	call("PUT", "users/by-id/"+userID("bob"), "users/bob", `{}`)
	return out
}

// statusLine renders a call's outcome; a refusal keeps its message, which is part of the behaviour.
func statusLine(method, label string, code int, body []byte) string {
	line := "http " + method + " " + label + " " + itoa(code)
	if code >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		line += " " + uuidPattern.ReplaceAllString(e.Error.Message, "<id>")
	}
	return line
}

//go:build integration

// apply_test.go covers the declarative surface end to end: the plan actions
// POST /api/apply reports, the prune guard rails, the all-or-nothing
// authorization step and the partial-write report, the boot seed running the
// same loader, and the export → apply round trip.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/seed"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/ids"
)

// bundle is the fixture manifest: one team, its project, and rows owned by
// each — enough to exercise ordering, ownership, and cross-refs.
const bundle = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata:
  name: platform
  displayName: Platform
  owner: {kind: user}
  labels: {managed: gitops}
spec:
  enabled: true
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata:
  name: ml-search
  displayName: ML Search
  labels: {managed: gitops}
spec:
  team: platform
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata:
  name: ml-search-default
  owner: {kind: project, name: ml-search}
  labels: {managed: gitops}
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: ServiceAccount
metadata:
  name: search-indexer
  labels: {managed: gitops}
spec:
  project: ml-search
  policy: ml-search-default
`

// globalBinding is a RoleBinding at the global scope. Its owner mirrors the
// scope, so the row is system-owned while still being tenant-authored — the
// case export and prune must not mistake for a catalog row.
const globalBinding = `---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata:
  name: global-admins
  labels: {managed: gitops}
spec:
  role: admin
  scope: {kind: system}
  subjects:
    - {kind: group, name: platform-eng}
`

type applyResp struct {
	Plan []struct {
		Kind          string   `json:"kind"`
		Name          string   `json:"name"`
		Action        string   `json:"action"`
		ChangedFields []string `json:"changedFields"`
	} `json:"plan"`
	Applied bool `json:"applied"`
	Counts  struct {
		Create, Update, Unchanged, SkipDirty, Delete int
	} `json:"counts"`
}

// applyBundle POSTs a YAML bundle and decodes the plan.
func (s *stack) applyBundle(yaml, query string) (int, applyResp, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.control.URL+"/api/apply?"+query, bytes.NewReader([]byte(yaml)))
	if err != nil {
		s.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("apply: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out applyResp
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func (r applyResp) action(kind, name string) string {
	for _, e := range r.Plan {
		if e.Kind == kind && e.Name == name {
			return e.Action
		}
	}
	return ""
}

func (r applyResp) changed(kind, name string) []string {
	for _, e := range r.Plan {
		if e.Kind == kind && e.Name == name {
			return e.ChangedFields
		}
	}
	return nil
}

func TestIntegration_ApplyPlanActions(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()

	// dryRun plans creates and writes nothing.
	code, plan, raw := st.applyBundle(bundle, "dryRun=true")
	if code != http.StatusOK {
		t.Fatalf("dry run: %d %s", code, raw)
	}
	if plan.Applied || plan.Counts.Create != 4 {
		t.Fatalf("dry run plan = %+v", plan.Counts)
	}
	teams, err := st.stores.Team.List(ctx)
	if err != nil || len(teams) != 0 {
		t.Fatalf("dry run wrote rows: %v %d", err, len(teams))
	}

	// The same bundle applied for real.
	code, plan, raw = st.applyBundle(bundle, "")
	if code != http.StatusOK || !plan.Applied {
		t.Fatalf("apply: %d %s", code, raw)
	}
	if plan.Counts.Create != 4 {
		t.Fatalf("apply counts = %+v", plan.Counts)
	}
	teams, err = st.stores.Team.List(ctx)
	if err != nil || len(teams) != 1 {
		t.Fatalf("list teams: %v %d", err, len(teams))
	}

	// Re-applying an unchanged bundle is a no-op for every row.
	_, plan, _ = st.applyBundle(bundle, "")
	if plan.Counts.Unchanged != 4 {
		t.Fatalf("re-apply counts = %+v (plan %+v)", plan.Counts, plan.Plan)
	}

	// An edited field is reported by path.
	edited := strings.Replace(bundle, "displayName: Platform", "displayName: Platform Engineering", 1)
	_, plan, _ = st.applyBundle(edited, "")
	if got := plan.action("Team", "platform"); got != "update" {
		t.Fatalf("edited team action = %q", got)
	}
	if got := plan.changed("Team", "platform"); len(got) != 1 || got[0] != "metadata.displayName" {
		t.Fatalf("changedFields = %v", got)
	}

	// An operator edit through the API marks the row dirty; apply reports
	// the drift and leaves it alone.
	teams, _ = st.stores.Team.List(ctx)
	teams[0].Meta.Dirty = true
	teams[0].Meta.DisplayName = "Operator Edited"
	if err := st.stores.Team.Upsert(ctx, teams[0]); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	_, plan, _ = st.applyBundle(bundle, "")
	if got := plan.action("Team", "platform"); got != "skip-dirty" {
		t.Fatalf("dirty team action = %q", got)
	}
	after, _ := st.stores.Team.Get(ctx, teams[0].Meta.ID)
	if after.Meta.DisplayName != "Operator Edited" {
		t.Fatalf("dirty row was clobbered: %q", after.Meta.DisplayName)
	}

	// force takes the row back and clears the flag.
	_, plan, _ = st.applyBundle(bundle, "force=true")
	if got := plan.action("Team", "platform"); got != "update" {
		t.Fatalf("forced team action = %q", got)
	}
	after, _ = st.stores.Team.Get(ctx, teams[0].Meta.ID)
	if after.Meta.DisplayName != "Platform" || after.Meta.Dirty {
		t.Fatalf("force left %q dirty=%v", after.Meta.DisplayName, after.Meta.Dirty)
	}
}

func TestIntegration_ApplyPrune(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()

	seedBuiltinRoles(t, st)
	if code, _, raw := st.applyBundle(bundle+globalBinding, ""); code != http.StatusOK {
		t.Fatalf("seed bundle: %d %s", code, raw)
	}

	// A system-owned catalog row carrying the same label: prune must not
	// touch it however well it matches.
	prov := &provider.Provider{Meta: meta.Metadata{
		ID: ids.New(), Name: "acme", DisplayName: "Acme",
		Owner: meta.Owner{Kind: meta.OwnerSystem}, Labels: map[string]string{"managed": "gitops"},
	}}
	if err := st.stores.Provider.Upsert(ctx, prov); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// Prune with no selector is refused outright.
	code, _, raw := st.applyBundle(bundle, "prune=true")
	if code != http.StatusBadRequest {
		t.Fatalf("prune without selector: %d %s", code, raw)
	}

	// Drop the service account from the bundle; prune should remove it and
	// nothing else.
	parts := strings.Split(bundle, "---\n")
	trimmed := strings.Join(parts[:len(parts)-1], "---\n")
	code, plan, raw := st.applyBundle(trimmed, "prune=true&selector=managed=gitops")
	if code != http.StatusOK {
		t.Fatalf("prune: %d %s", code, raw)
	}
	// Both the omitted service account and the omitted global binding go;
	// the binding's system owner mirrors its scope and must not shield it.
	if plan.Counts.Delete != 2 ||
		plan.action("ServiceAccount", "search-indexer") != "delete" ||
		plan.action("RoleBinding", "global-admins") != "delete" {
		t.Fatalf("prune plan = %+v (%+v)", plan.Counts, plan.Plan)
	}
	if bindings, err := st.stores.RoleBinding.List(ctx); err != nil || len(bindings) != 0 {
		t.Fatalf("global binding survived prune: %v %d", err, len(bindings))
	}
	sas, err := st.stores.ServiceAccount.List(ctx)
	if err != nil || len(sas) != 0 {
		t.Fatalf("service account survived prune: %v %d", err, len(sas))
	}
	if got, err := st.stores.Provider.Get(ctx, prov.Meta.ID); err != nil || got == nil {
		t.Fatalf("prune deleted a system-owned row: %v", err)
	}
	if teams, _ := st.stores.Team.List(ctx); len(teams) != 1 {
		t.Fatalf("prune deleted a declared row")
	}
}

// denyAll refuses one kind, standing in for an RBAC decision.
type denyAll struct{ kind string }

func (d denyAll) Authorize(_ context.Context, _ string, res authz.Resource) error {
	if res.Kind == d.kind {
		return authz.ErrForbidden
	}
	return nil
}

func TestIntegration_ApplyAuthorizationIsAllOrNothing(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	docs := parseBundle(t, bundle)

	plan, err := apply.Plan(ctx, docs, apply.Options{Stores: applyStores(st)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The denied kind sorts last in the plan; nothing before it may land.
	applied, err := apply.Execute(ctx, plan, denyAll{kind: "service-account"})
	var ae *apply.AuthzError
	if !errors.As(err, &ae) {
		t.Fatalf("Execute err = %v, want an AuthzError", err)
	}
	if len(applied) != 0 {
		t.Fatalf("a denied apply wrote %d rows", len(applied))
	}
	if teams, _ := st.stores.Team.List(ctx); len(teams) != 0 {
		t.Fatalf("a denied apply wrote %d teams", len(teams))
	}
}

func TestIntegration_ApplyPartialStoreFailureReportsWhatLanded(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	docs := parseBundle(t, bundle)

	// A store bundle whose project store is backed by a closed pool: the
	// team write lands, the project write fails, and the error names both.
	second, err := pgxpool.New(ctx, os.Getenv("RELAY_TEST_PG_DSN"))
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	stores := applyStores(st)
	stores.Project = apply.NewStores(second, nil).Project

	plan, err := apply.Plan(ctx, docs, apply.Options{Stores: stores})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// Only the project store's pool dies, so the team write lands and the
	// project write is the one that fails.
	second.Close()
	applied, err := apply.Execute(ctx, plan, nil)
	var se *apply.StoreError
	if !errors.As(err, &se) {
		t.Fatalf("Execute err = %v, want a StoreError", err)
	}
	if se.Entry.Kind != "Project" {
		t.Fatalf("failed entry = %+v, want the Project", se.Entry)
	}
	if len(applied) != 1 || applied[0].Kind != "Team" {
		t.Fatalf("applied = %+v, want just the Team", applied)
	}
	if teams, _ := st.stores.Team.List(ctx); len(teams) != 1 {
		t.Fatalf("the row reported as applied is not in PG")
	}
}

// The boot seed and an apply of the same directory must converge on the
// same rows — they are one loader.
func TestIntegration_SeedMatchesApply(t *testing.T) {
	st := newStack(t)
	ctx := context.Background()
	pool := testPool(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle.yaml"), []byte(bundle), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	if _, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seeded := goldenRows(t, st)

	store, err := storagemod.Open(ctx, os.Getenv("RELAY_TEST_PG_DSN"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	truncateAll(t, store)

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		t.Fatalf("load dir: %v", err)
	}
	plan, err := apply.Plan(ctx, docs, apply.Options{Stores: applyStores(st)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := apply.Execute(ctx, plan, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	applied := goldenRows(t, st)

	if len(seeded) != len(applied) {
		t.Fatalf("seed wrote %d rows, apply wrote %d", len(seeded), len(applied))
	}
	for k, want := range seeded {
		if applied[k] != want {
			t.Fatalf("row %s differs:\n seed: %s\napply: %s", k, want, applied[k])
		}
	}

	// And the seed subcommand's own path still reports what it reconciled.
	res, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir})
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if res.Teams != 1 || res.Projects != 1 || res.Policies != 1 || res.ServiceAccounts != 1 {
		t.Fatalf("re-seed result = %+v", res)
	}
}

func TestIntegration_ExportRoundTripsThroughApply(t *testing.T) {
	st := newStack(t)
	seedBuiltinRoles(t, st)
	if code, _, raw := st.applyBundle(bundle+globalBinding, ""); code != http.StatusOK {
		t.Fatalf("seed bundle: %d %s", code, raw)
	}

	code, body := st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, body)
	}
	out := string(body)

	// Secrets and rotation state never leave.
	for _, forbidden := range []string{"previousKeyHash", "graceUntil", "\n  value:", "value: sk-"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("export leaked %q:\n%s", forbidden, out)
		}
	}
	// Every document carries its schema directive and its server-owned id.
	if strings.Count(out, "# yaml-language-server: $schema=/api/schemas/v1alpha2/") != 5 {
		t.Fatalf("expected one schema directive per document:\n%s", out)
	}
	// A global binding is tenant-authored and belongs in the bundle; the
	// built-in roles it names are the relay's own rows and stay out.
	if !strings.Contains(out, "name: global-admins") {
		t.Fatalf("export dropped the global role binding:\n%s", out)
	}
	if strings.Contains(out, "\nkind: Role\n") {
		t.Fatalf("export included a built-in role:\n%s", out)
	}
	if !strings.Contains(out, "  id: ") {
		t.Fatalf("export omitted metadata.id:\n%s", out)
	}
	// Catalog template rows stay out of the bundle.
	if strings.Contains(out, "kind: Provider") || strings.Contains(out, "kind: Model") {
		t.Fatalf("export included catalog template rows:\n%s", out)
	}
	// Rows are ordered tenancy-first so a replay never forward-references.
	if i, j := strings.Index(out, "kind: Team"), strings.Index(out, "kind: Project"); i > j {
		t.Fatalf("export ordered projects before teams:\n%s", out)
	}

	// The bundle it produced applies back as a no-op.
	_, plan, raw := st.applyBundle(out, "dryRun=true")
	for _, e := range plan.Plan {
		if e.Action != "unchanged" {
			t.Fatalf("round trip reported %s on %s/%s (%v)\n%s", e.Action, e.Kind, e.Name, e.ChangedFields, raw)
		}
	}
	if plan.Counts.Unchanged != 5 {
		t.Fatalf("round trip counts = %+v", plan.Counts)
	}
	if plan.action("RoleBinding", "global-admins") != "unchanged" {
		t.Fatalf("global binding did not round-trip: %+v", plan.Plan)
	}

	// Every exported kind is a kind the schema endpoint serves, so the
	// directive above resolves.
	for _, line := range strings.Split(out, "\n") {
		kind, ok := strings.CutPrefix(line, "kind: ")
		if !ok {
			continue
		}
		code, raw := st.adminDo(http.MethodGet, "/api/schemas/v1alpha2/"+kind, "")
		if code != http.StatusOK {
			t.Fatalf("schema for exported kind %q: %d %s", kind, code, raw)
		}
	}
}

func TestIntegration_ExportScopeAndKinds(t *testing.T) {
	st := newStack(t)
	if code, _, raw := st.applyBundle(bundle, ""); code != http.StatusOK {
		t.Fatalf("seed bundle: %d %s", code, raw)
	}
	projects, _ := st.stores.Project.List(context.Background())
	if len(projects) != 1 {
		t.Fatalf("expected one project")
	}

	code, body := st.adminDo(http.MethodGet, "/api/export?kinds=teams", "")
	if code != http.StatusOK {
		t.Fatalf("export kinds: %d %s", code, body)
	}
	if n := strings.Count(string(body), "\nkind: "); n != 1 || !strings.Contains(string(body), "\nkind: Team") {
		t.Fatalf("kinds=teams returned:\n%s", body)
	}

	code, body = st.adminDo(http.MethodGet, "/api/export?scope=project:"+projects[0].Meta.ID, "")
	if code != http.StatusOK {
		t.Fatalf("export scope: %d %s", code, body)
	}
	if strings.Contains(string(body), "kind: Team") {
		t.Fatalf("project scope included the team:\n%s", body)
	}
	if !strings.Contains(string(body), "name: ml-search") {
		t.Fatalf("project scope dropped its own project:\n%s", body)
	}

	if code, body = st.adminDo(http.MethodGet, "/api/export?kinds=models", ""); code != http.StatusBadRequest {
		t.Fatalf("non-exportable kind: %d %s", code, body)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func parseBundle(t *testing.T, yaml string) []manifest.Document {
	t.Helper()
	docs, err := manifest.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse bundle: %v", err)
	}
	return docs
}

func applyStores(st *stack) *apply.Stores {
	return &apply.Stores{
		Provider: st.stores.Provider, Host: st.stores.Host, RateLimit: st.stores.RateLimit,
		HostKey: st.stores.HostKey, Model: st.stores.Model, Policy: st.stores.Policy,
		Pricing: st.stores.Pricing, HostBinding: st.stores.Binding, Key: st.stores.Key,
		Team: st.stores.Team, Project: st.stores.Project,
		ServiceAccount: st.stores.ServiceAccount, Group: st.stores.Group, Role: st.stores.Role,
		RoleBinding: st.stores.RoleBinding, PolicyBinding: st.stores.PolicyBinding,
		Overlay: st.stores.Overlay, User: st.users,
	}
}

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// goldenRows renders every row as "kind/name" -> JSON, with server-stamped
// state dropped and ids replaced by the row they point at, so two runs that
// minted different UUIDs for the same manifest compare equal.
func goldenRows(t *testing.T, st *stack) map[string]string {
	t.Helper()
	rows, err := apply.Load(context.Background(), applyStores(st))
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	type entry struct {
		kind, id, name string
		row            any
	}
	var all []entry
	for _, x := range rows.Teams {
		all = append(all, entry{"Team", x.Meta.ID, x.Meta.Name, x})
	}
	for _, x := range rows.Projects {
		all = append(all, entry{"Project", x.Meta.ID, x.Meta.Name, x})
	}
	for _, x := range rows.Policies {
		all = append(all, entry{"Policy", x.Meta.ID, x.Meta.Name, x})
	}
	for _, x := range rows.ServiceAccounts {
		all = append(all, entry{"ServiceAccount", x.Meta.ID, x.Meta.Name, x})
	}

	names := map[string]string{}
	for _, e := range all {
		names[e.id] = e.kind + ":" + e.name
	}
	out := map[string]string{}
	for _, e := range all {
		blob, err := json.Marshal(e.row)
		if err != nil {
			t.Fatalf("marshal %s/%s: %v", e.kind, e.name, err)
		}
		var generic map[string]any
		if err := json.Unmarshal(blob, &generic); err != nil {
			t.Fatalf("unmarshal %s/%s: %v", e.kind, e.name, err)
		}
		if m, ok := generic["metadata"].(map[string]any); ok {
			delete(m, "createdAt")
			delete(m, "updatedAt")
		}
		blob, _ = json.Marshal(generic)
		out[e.kind+"/"+e.name] = uuidPattern.ReplaceAllStringFunc(string(blob), func(id string) string {
			if n, ok := names[id]; ok {
				return n
			}
			return "<unknown-id>"
		})
	}
	return out
}

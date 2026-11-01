//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// A database with a subject already in two policy bindings stops the upgrade with a message naming the subject and both bindings, rather than failing on the unique index.
func TestMigrateUpRefusesASubjectInTwoBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := st.Pool()
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "dup-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	prj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "dup-project", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "dup-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "dup-sa"}, Spec: serviceaccount.Spec{ProjectID: prj.Meta.ID}}
	sa.StampOwner()
	binding := func(name string, sub rolebinding.Subject) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: policybinding.Spec{ProjectID: prj.Meta.ID, PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{sub}},
		}
	}
	first := binding("first-binding", rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: sa.Meta.ID})
	second := binding("second-binding", rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"})
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"team", func() error { return team.NewStore(db).Upsert(ctx, tm) }},
		{"project", func() error { return project.NewStore(db).Upsert(ctx, prj) }},
		{"service account", func() error { return serviceaccount.NewStore(db).Upsert(ctx, sa) }},
		{"policy", func() error { return policy.NewStore(db).Upsert(ctx, pol) }},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("upsert %s: %v", step.name, err)
		}
	}
	for _, b := range []*policybinding.PolicyBinding{first, second} {
		if err := policybinding.NewStore(db).Upsert(ctx, b); err != nil {
			t.Fatalf("upsert binding %s: %v", b.Meta.Name, err)
		}
	}
	st.Close()

	// Below 000035 nothing holds a subject to one binding.
	if err := storagemod.MigrateTo(dsn, 34); err != nil {
		t.Fatalf("migrate down to 34: %v", err)
	}
	conn := testPool(t, dsn)
	if _, err := conn.Exec(ctx, "INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_sa_id, position) VALUES ($1, 'serviceaccount', $2, $2, 1)", second.Meta.ID, sa.Meta.ID); err != nil {
		t.Fatalf("bind the service account twice: %v", err)
	}
	err = storagemod.MigrateUp(dsn)
	if err == nil {
		t.Fatal("the upgrade went through with a subject in two bindings")
	}
	for _, want := range []string{"serviceaccount " + sa.Meta.ID, "first-binding", "second-binding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("upgrade error %q does not name %q", err, want)
		}
	}
}

// Upgrading over host keys without an account gives each its owner's default account at its host: one per owner and host, named after the host and suffixed when taken. A named account is kept; a key whose host is gone is left alone.
func TestMigrateUpGivesKeysTheirOwnersDefaultAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	q := gen.New(st.Pool())
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", DisplayName: "Acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	taken := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: hst.Meta.ID}}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	if err := account.NewStore(q).Upsert(ctx, taken); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	st.Close()

	if err := storagemod.MigrateTo(dsn, 35); err != nil {
		t.Fatalf("migrate down to 35: %v", err)
	}
	conn := testPool(t, dsn)
	alice := meta.NewID()
	ids := map[string]string{}
	for _, k := range []struct{ name, owner, spec string }{
		{"sys-a", `{"owner":{"kind":"system"}}`, `{"hostId":"` + hst.Meta.ID + `"}`},
		{"sys-b", `{"owner":{"kind":"system"}}`, `{"hostId":"` + hst.Meta.ID + `","accountId":""}`},
		{"alice", `{"owner":{"kind":"user","id":"` + alice + `"}}`, `{"hostId":"` + hst.Meta.ID + `"}`},
		{"named", `{"owner":{"kind":"system"}}`, `{"hostId":"` + hst.Meta.ID + `","accountId":"` + taken.Meta.ID + `"}`},
		{"no-host", `{"owner":{"kind":"system"}}`, `{"hostId":"` + meta.NewID() + `"}`},
	} {
		ids[k.name] = meta.NewID()
		if _, err := conn.Exec(ctx, "INSERT INTO secrets (id, name, value_kind, value_from_env, metadata, spec) VALUES ($1, $2, 'env', 'ACME_API_KEY', $3, $4)",
			ids[k.name], k.name, k.owner, k.spec); err != nil {
			t.Fatalf("insert key %s: %v", k.name, err)
		}
	}
	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	accountOf := func(name string) string {
		t.Helper()
		var id string
		if err := conn.QueryRow(ctx, "SELECT COALESCE(spec->>'accountId', '') FROM secrets WHERE id = $1", ids[name]).Scan(&id); err != nil {
			t.Fatalf("read key %s: %v", name, err)
		}
		return id
	}
	sys, ali := accountOf("sys-a"), accountOf("alice")
	if sys == "" || sys != accountOf("sys-b") {
		t.Errorf("system keys joined %q and %q, want one default account", sys, accountOf("sys-b"))
	}
	if ali == "" || ali == sys {
		t.Errorf("alice's key joined %q, want her own default account", ali)
	}
	if got := accountOf("named"); got != taken.Meta.ID {
		t.Errorf("named key's account = %q, want %q kept", got, taken.Meta.ID)
	}
	if got := accountOf("no-host"); got != "" {
		t.Errorf("key without a host joined %q, want none", got)
	}

	// The host's own name is taken, so the two defaults take the next free suffixes, in key order.
	accounts := account.NewStore(gen.New(conn))
	names := map[string]bool{}
	for _, tc := range []struct {
		id    string
		owner meta.Owner
	}{
		{sys, meta.Owner{Kind: meta.OwnerSystem}},
		{ali, meta.Owner{Kind: meta.OwnerUser, ID: alice}},
	} {
		a, err := accounts.Get(ctx, tc.id)
		if err != nil || a == nil {
			t.Fatalf("Get default account %s: %v (row %v)", tc.id, err, a)
		}
		names[a.Meta.Name] = true
		if a.Meta.DisplayName != "Acme" || a.Meta.Owner != tc.owner || !a.Spec.Default || a.Spec.HostID != hst.Meta.ID {
			t.Errorf("default account = %+v %+v, want one owned by %+v", a.Meta, a.Spec, tc.owner)
		}
		// The store finds the account the migration made.
		if id, err := accounts.EnsureDefault(ctx, tc.owner, hst.Meta.ID); err != nil || id != tc.id {
			t.Errorf("EnsureDefault(%+v) = %q, %v; want %q", tc.owner, id, err, tc.id)
		}
	}
	if !names["acme-2"] || !names["acme-3"] {
		t.Errorf("default account names = %v, want acme-2 and acme-3", names)
	}
}

// A team's or project's own budget nothing enforced is dropped on the upgrade; the rest of the spec stays.
func TestMigrateUpDropsTeamAndProjectBudgets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	if err := storagemod.MigrateTo(dsn, 35); err != nil {
		t.Fatalf("migrate to 35: %v", err)
	}
	conn := testPool(t, dsn)
	teamID, projectID := meta.NewID(), meta.NewID()
	if _, err := conn.Exec(ctx, `INSERT INTO teams (id, name, metadata, spec) VALUES ($1, 'budgeted', '{"owner":{"kind":"system"}}', '{"enabled":false,"budget":{"amount":"10.00","period":"month"}}')`, teamID); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO projects (id, name, team_id, metadata, spec) VALUES ($1, 'budgeted', $2, '{"owner":{"kind":"team"}}', $3)`,
		projectID, teamID, `{"teamId":"`+teamID+`","budget":{"amount":"5","period":"week"}}`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	for table, want := range map[string]string{
		"teams":    `{"enabled": false}`,
		"projects": `{"teamId": "` + teamID + `"}`,
	} {
		var spec string
		if err := conn.QueryRow(ctx, "SELECT spec::text FROM "+table+" LIMIT 1").Scan(&spec); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if spec != want {
			t.Errorf("%s spec = %s, want %s", table, spec, want)
		}
	}
}

// lockedBuffer is a log sink safe to read while another goroutine may still write to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The upgrade logs, by name, each pre-0.20 link it drops: a key's, service account's or host key's policy, and a policy's host keys, models, rate limit and per-model rate limits; and each binding that stops governing as before: group subjects, and users under a project's binding. A link to a row that is gone is named by id.
// Not parallel: it reads the process-wide default logger.
func TestMigrateUpLogsTheLinksItDrops(t *testing.T) {
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	if err := storagemod.MigrateTo(dsn, 35); err != nil {
		t.Fatalf("migrate to 35: %v", err)
	}
	conn := testPool(t, dsn)
	system := `{"owner":{"kind":"system"}}`
	userID, hostID, modelID, rlID, rlModelsID, tierID, gone := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	hostKeyID := meta.NewID()
	teamID, projectID, plainID, bindingID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	for _, stmt := range []struct {
		what, sql string
		args      []any
	}{
		{"user", `INSERT INTO users (id, username) VALUES ($1, 'link-user')`, []any{userID}},
		{"host", `INSERT INTO hosts (id, name, metadata, spec) VALUES ($1, 'link-host', $2, '{"baseUrl":"https://api.example.com"}')`, []any{hostID, system}},
		{"model", `INSERT INTO models (id, name, metadata, spec) VALUES ($1, 'link-model', $2, '{}')`, []any{modelID, system}},
		{"rate limit", `INSERT INTO rate_limits (id, name, metadata, spec) VALUES ($1, 'tier-rpm', $2, '{}')`, []any{rlID, system}},
		{"model rate limit", `INSERT INTO rate_limits (id, name, metadata, spec) VALUES ($1, 'model-rpm', $2, '{}')`, []any{rlModelsID, system}},
		{"policy", `INSERT INTO policies (id, name, metadata, spec, rate_limit_id) VALUES ($1, 'tier-one', $2, $3, $4)`, []any{
			tierID, system, `{"rlBindings":[{"models":["acme/*","acme/big"],"rateLimitId":"` + rlModelsID + `"}]}`, rlID,
		}},
		{"host key", `INSERT INTO secrets (id, name, value_kind, value_from_env, metadata, spec) VALUES ($1, 'link-host-key', 'env', 'LINK_KEY', $2, $3)`, []any{
			hostKeyID, system, `{"hostId":"` + hostID + `","policyId":"` + tierID + `"}`,
		}},
		{"policy host key", `INSERT INTO policy_host_keys (policy_id, host_key_id, position) VALUES ($1, $2, 0)`, []any{tierID, hostKeyID}},
		{"policy model", `INSERT INTO policy_models (policy_id, model_id, position) VALUES ($1, $2, 0)`, []any{tierID, modelID}},
		{"key", `INSERT INTO relay_keys (id, name, key_hash, principal_user_id, metadata, spec) VALUES ($1, 'link-key', 'aa11', $2, $3, $4)`, []any{
			meta.NewID(), userID, `{"owner":{"kind":"user","id":"` + userID + `"}}`, `{"policyId":"` + tierID + `","principal":{"kind":"","id":""}}`,
		}},
		{"key to a deleted policy", `INSERT INTO relay_keys (id, name, key_hash, principal_user_id, metadata, spec) VALUES ($1, 'stale-key', 'bb22', $2, $3, $4)`, []any{
			meta.NewID(), userID, `{"owner":{"kind":"user","id":"` + userID + `"}}`, `{"policyId":"` + gone + `"}`,
		}},
		{"key without a policy", `INSERT INTO relay_keys (id, name, key_hash, principal_user_id, metadata, spec) VALUES ($1, 'plain-key', 'cc33', $2, $3, '{}')`, []any{
			meta.NewID(), userID, `{"owner":{"kind":"user","id":"` + userID + `"}}`,
		}},
		{"team", `INSERT INTO teams (id, name, metadata, spec) VALUES ($1, 'link-team', $2, '{}')`, []any{teamID, system}},
		{"project", `INSERT INTO projects (id, name, team_id, metadata, spec) VALUES ($1, 'link-project', $2, $3, '{}')`, []any{projectID, teamID, `{"owner":{"kind":"team","id":"` + teamID + `"}}`}},
		{"plain policy", `INSERT INTO policies (id, name, metadata, spec) VALUES ($1, 'plain-policy', $2, '{}')`, []any{plainID, system}},
		{"service account with a policy", `INSERT INTO service_accounts (id, name, project_id, metadata, spec) VALUES ($1, 'link-sa', $2, $3, $4)`, []any{
			meta.NewID(), projectID, `{"owner":{"kind":"project","id":"` + projectID + `"}}`, `{"projectId":"` + projectID + `","policyId":"` + plainID + `"}`,
		}},
		{"binding", `INSERT INTO policy_bindings (id, name, project_id, policy_id, metadata, spec) VALUES ($1, 'link-binding', $2, $3, $4, '{}')`, []any{
			bindingID, projectID, plainID, `{"owner":{"kind":"project","id":"` + projectID + `"}}`,
		}},
		{"group subject", `INSERT INTO policy_binding_subjects (binding_id, kind, subject_name, position, owner_scope) VALUES ($1, 'group', 'system:authenticated', 0, $2)`, []any{bindingID, "project:" + projectID}},
		{"user subject", `INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_user_id, position, owner_scope) VALUES ($1, 'user', $2, $2, 1, $3)`, []any{bindingID, userID, "project:" + projectID}},
	} {
		if _, err := conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("insert %s: %v", stmt.what, err)
		}
	}

	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	slog.SetDefault(prev)

	var got []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if _, notice, ok := strings.Cut(line, "notice="); ok && strings.Contains(line, "000036") {
			msg, err := strconv.Unquote(strings.TrimSuffix(notice, " severity=NOTICE"))
			if err != nil {
				t.Fatalf("notice %s: %v", notice, err)
			}
			got = append(got, msg)
		}
	}
	want := []string{
		`000036: dropping link: key "link-key" -> policy "tier-one"`,
		`000036: dropping link: key "stale-key" -> policy "` + gone + `"`,
		`000036: dropping link: host key "link-host-key" -> tier policy "tier-one"`,
		`000036: dropping link: service account "link-sa" -> policy "plain-policy"`,
		`000036: binding no longer applies: policy binding "link-binding" (policy "plain-policy") -> group "system:authenticated"`,
		`000036: binding narrows to project tokens: policy binding "link-binding" (policy "plain-policy") -> user "link-user" governs only tokens in project "link-project", not personal keys`,
		`000036: dropping link: policy "tier-one" -> host key "link-host-key"`,
		`000036: dropping link: policy "tier-one" -> model "link-model"`,
		`000036: dropping link: policy "tier-one" -> rate limit "tier-rpm"`,
		`000036: dropping link: policy "tier-one" -> rate limit "model-rpm" for models acme/*, acme/big`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("notices:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A bound policy whose only model or host-key restriction is the pre-0.20 list stops the upgrade, named with its bindings, rather than serving every model and key after it; once unbound, the upgrade goes through. A policy bound only to a group, which binds nothing after the upgrade, does not stop it.
func TestMigrateUpRefusesWideningABoundPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	if err := storagemod.MigrateTo(dsn, 35); err != nil {
		t.Fatalf("migrate to 35: %v", err)
	}
	conn := testPool(t, dsn)
	system := `{"owner":{"kind":"system"}}`
	userID, hostID, modelID, hostKeyID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	teamID, projectID, saID := meta.NewID(), meta.NewID(), meta.NewID()
	byModels, byKeys, byGroup, narrowed := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	inProject := `{"owner":{"kind":"project","id":"` + projectID + `"}}`
	binding := func(policyID, name string) []any { return []any{meta.NewID(), name, projectID, policyID, inProject} }
	bindings := map[string][]any{
		"sa-binding":    binding(byModels, "sa-binding"),
		"user-binding":  binding(byKeys, "user-binding"),
		"group-binding": binding(byGroup, "group-binding"),
		"kept-binding":  binding(narrowed, "kept-binding"),
	}
	for _, stmt := range []struct {
		what, sql string
		args      []any
	}{
		{"user", `INSERT INTO users (id, username) VALUES ($1, 'widen-user')`, []any{userID}},
		{"host", `INSERT INTO hosts (id, name, metadata, spec) VALUES ($1, 'widen-host', $2, '{"baseUrl":"https://api.example.com"}')`, []any{hostID, system}},
		{"model", `INSERT INTO models (id, name, metadata, spec) VALUES ($1, 'widen-model', $2, '{}')`, []any{modelID, system}},
		{"host key", `INSERT INTO secrets (id, name, value_kind, value_from_env, metadata, spec) VALUES ($1, 'widen-host-key', 'env', 'WIDEN_KEY', $2, $3)`, []any{hostKeyID, system, `{"hostId":"` + hostID + `"}`}},
		{"team", `INSERT INTO teams (id, name, metadata, spec) VALUES ($1, 'widen-team', $2, '{}')`, []any{teamID, system}},
		{"project", `INSERT INTO projects (id, name, team_id, metadata, spec) VALUES ($1, 'widen-project', $2, $3, '{}')`, []any{projectID, teamID, `{"owner":{"kind":"team","id":"` + teamID + `"}}`}},
		{"service account", `INSERT INTO service_accounts (id, name, project_id, metadata, spec) VALUES ($1, 'widen-sa', $2, $3, $4)`, []any{saID, projectID, inProject, `{"projectId":"` + projectID + `"}`}},
		{"policy by model ids", `INSERT INTO policies (id, name, metadata, spec) VALUES ($1, 'by-model-ids', $2, '{}')`, []any{byModels, system}},
		{"policy by host keys", `INSERT INTO policies (id, name, metadata, spec) VALUES ($1, 'by-host-keys', $2, '{}')`, []any{byKeys, system}},
		{"policy bound to a group", `INSERT INTO policies (id, name, metadata, spec) VALUES ($1, 'by-group', $2, '{}')`, []any{byGroup, system}},
		{"policy keeping model refs", `INSERT INTO policies (id, name, metadata, spec, models) VALUES ($1, 'keeps-refs', $2, '{}', '["acme/*"]')`, []any{narrowed, system}},
		{"model ids", `INSERT INTO policy_models (policy_id, model_id, position) VALUES ($1, $3, 0), ($2, $3, 0), ($4, $3, 0)`, []any{byModels, byGroup, modelID, narrowed}},
		{"host keys", `INSERT INTO policy_host_keys (policy_id, host_key_id, position) VALUES ($1, $2, 0)`, []any{byKeys, hostKeyID}},
	} {
		if _, err := conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("insert %s: %v", stmt.what, err)
		}
	}
	for name, args := range bindings {
		if _, err := conn.Exec(ctx, `INSERT INTO policy_bindings (id, name, project_id, policy_id, metadata, spec) VALUES ($1, $2, $3, $4, $5, '{}')`, args...); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	scope := "project:" + projectID
	for what, stmt := range map[string]struct {
		sql  string
		args []any
	}{
		"service account subject": {`INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_sa_id, position, owner_scope) VALUES ($1, 'serviceaccount', $2, $2, 0, $3)`, []any{bindings["sa-binding"][0], saID, scope}},
		"user subject":            {`INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_user_id, position, owner_scope) VALUES ($1, 'user', $2, $2, 0, $3)`, []any{bindings["user-binding"][0], userID, scope}},
		"group subject":           {`INSERT INTO policy_binding_subjects (binding_id, kind, subject_name, position, owner_scope) VALUES ($1, 'group', 'system:authenticated', 0, $2)`, []any{bindings["group-binding"][0], scope}},
		"project subject":         {`INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_project_id, position, owner_scope) VALUES ($1, 'project', $2, $2, 0, $3)`, []any{bindings["kept-binding"][0], projectID, scope}},
	} {
		if _, err := conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("insert %s: %v", what, err)
		}
	}

	err := storagemod.MigrateUp(dsn)
	if err == nil {
		t.Fatal("the upgrade went through with bound policies losing their only restriction")
	}
	for _, want := range []string{"policy by-model-ids loses its model ids (bound by sa-binding)", "policy by-host-keys loses its host keys (bound by user-binding)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("upgrade error %q does not name %q", err, want)
		}
	}
	for _, unwanted := range []string{"by-group", "keeps-refs"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("upgrade error %q names %q, which loses nothing a subject relied on", err, unwanted)
		}
	}

	if _, err := conn.Exec(ctx, `DELETE FROM policy_bindings WHERE name IN ('sa-binding', 'user-binding')`); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	// The refused step left nothing applied, only the dirty flag the upgrade guide clears.
	if err := storagemod.ForceVersion(dsn, 35); err != nil {
		t.Fatalf("clear the failed step: %v", err)
	}
	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("upgrade after unbinding: %v", err)
	}
}

// Rolling back below accounts, pools and budgets drops their tables and the policy links to them, and keeps the policy bindings, minus the subjects the older schema cannot hold.
func TestMigrateDownBelowAccountsKeepsBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := st.Pool()
	q := gen.New(db)

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	prj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "eval", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "h", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	acct := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: hst.Meta.ID}}
	pl := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "main", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: pool.Spec{HostID: hst.Meta.ID}}
	bg := &budget.Budget{Meta: meta.Metadata{ID: meta.NewID(), Name: "monthly", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: budget.Spec{AmountUSD: "10"}}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "contractors", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{PoolIDs: []string{pl.Meta.ID}, BudgetIDs: []string{bg.Meta.ID}},
	}
	group := rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}
	pb := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "everyone"},
		Spec: policybinding.Spec{ProjectID: prj.Meta.ID, PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{
			{Kind: policybinding.SubjectHost, ID: hst.Meta.ID},
			group,
			{Kind: policybinding.SubjectAccount, ID: acct.Meta.ID},
		}},
	}
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"team", func() error { return team.NewStore(db).Upsert(ctx, tm) }},
		{"project", func() error { return project.NewStore(db).Upsert(ctx, prj) }},
		{"host", func() error { return host.NewStore(q).Upsert(ctx, hst) }},
		{"account", func() error { return account.NewStore(q).Upsert(ctx, acct) }},
		{"pool", func() error { return pool.NewStore(db).Upsert(ctx, pl) }},
		{"budget", func() error { return budget.NewStore(q).Upsert(ctx, bg) }},
		{"policy", func() error { return policy.NewStore(db).Upsert(ctx, pol) }},
		{"binding", func() error { return policybinding.NewStore(db).Upsert(ctx, pb) }},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("upsert %s: %v", step.name, err)
		}
	}
	st.Close()

	if err := storagemod.MigrateTo(dsn, 34); err != nil {
		t.Fatalf("migrate down to 34: %v", err)
	}
	if v, dirty := schemaVersion(t, dsn); v != 34 || dirty {
		t.Fatalf("schema after the down migration = %d dirty=%v, want 34 clean", v, dirty)
	}
	conn := testPool(t, dsn)
	for _, table := range []string{"accounts", "pools", "pool_credentials", "budgets", "policy_pools", "policy_credentials", "policy_rate_limits", "policy_budgets"} {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || exists {
			t.Errorf("table %s after the down migration: exists=%v err=%v", table, exists, err)
		}
	}
	var kinds []string
	rows, err := conn.Query(ctx, "SELECT kind FROM policy_binding_subjects WHERE binding_id = $1 ORDER BY position", pb.Meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	rows.Close()
	if len(kinds) != 1 || kinds[0] != string(rolebinding.SubjectGroup) {
		t.Errorf("subjects after the down migration = %v, want only the group", kinds)
	}

	st, err = storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen at head: %v", err)
	}
	st.Close()
	if v, dirty := schemaVersion(t, dsn); v != storagetest.LatestVersion(t) || dirty {
		t.Errorf("schema after coming back up = %d dirty=%v, want head", v, dirty)
	}
}

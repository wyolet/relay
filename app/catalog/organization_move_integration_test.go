//go:build integration

package catalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/catalogstore"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

var system = meta.Owner{Kind: meta.OwnerSystem}

// tableRows reads cols of every row of table keyed by id, as JSON text.
func tableRows(t *testing.T, db *pgxpool.Pool, table, cols string) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(), "SELECT id::text, row_to_json(x)::text FROM (SELECT id, "+cols+" FROM "+table+") x")
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out[id] = row
	}
	return out
}

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func ownerKind(t *testing.T, db *pgxpool.Pool, table, id string) string {
	t.Helper()
	var kind string
	if err := db.QueryRow(context.Background(), "SELECT COALESCE(metadata->'owner'->>'kind', '') FROM "+table+" WHERE id = $1", id).Scan(&kind); err != nil {
		t.Fatalf("owner of %s %s: %v", table, id, err)
	}
	return kind
}

// movedTables are the tables migration 45 changes, with the columns besides the owner that must come through it unchanged; down must restore every column.
var movedTables = map[string]string{
	"teams":           "name, display_name, metadata - 'owner' AS metadata, spec",
	"groups":          "name, display_name, metadata - 'owner' AS metadata, spec",
	"roles":           "name, display_name, metadata - 'owner' AS metadata, spec",
	"role_bindings":   "name, display_name, role_id, scope_id, metadata - 'owner' AS metadata, spec",
	"policy_bindings": "name, display_name, policy_id, project_id, team_id, user_id, metadata - 'owner' AS metadata, spec",
	"accounts":        "name, display_name, host_id, kind, metadata - 'owner' AS metadata, spec",
	"policies":        "name, display_name, metadata - 'owner' AS metadata, spec",
}

// Migration 45 moves the deployment's rows the owner alone decides and leaves every other column as it was; the boot moves the operator's policy; a global admin binding still grants at the global scope; and down restores schema 44's rows column for column.
func TestIntegration_Migration45RoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	if err := storage.MigrateTo(dsn, 44); err != nil {
		t.Fatalf("migrate down to 44 on empty tables: %v", err)
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	q := gen.New(db)

	// Schema 44's rows, written as a relay of that schema stored them.
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	if err := catalogstore.NewHostStore(q).Upsert(ctx, hst); err != nil {
		t.Fatal(err)
	}
	acct := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-org", Owner: system}, Spec: account.Spec{HostID: hst.Meta.ID}}
	if err := account.NewStore(q).Upsert(ctx, acct); err != nil {
		t.Fatal(err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-default", Owner: system}}
	if err := catalogstore.NewPolicyStore(db).Upsert(ctx, pol); err != nil {
		t.Fatal(err)
	}
	pb := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-default", Owner: system}, Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{acct.Meta.ID}}}
	if err := policybinding.NewStore(db).Upsert(ctx, pb); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{"team", "personal-team", "group", "admin-role", "custom-role", "global-binding", "team-binding", "admin-user"} {
		ids[name] = meta.NewID()
	}
	exec(t, db, `INSERT INTO users (id, username) VALUES ($1, 'ops')`, ids["admin-user"])
	exec(t, db, `INSERT INTO teams (id, name, metadata, spec) VALUES ($1, 'platform', '{"owner": {"kind": "system"}, "labels": {"a": "b"}}', '{}'), ($2, 'mine', $3::jsonb, '{}')`,
		ids["team"], ids["personal-team"], `{"owner": {"kind": "user", "id": "`+ids["admin-user"]+`"}}`)
	exec(t, db, `INSERT INTO groups (id, name, metadata, spec) VALUES ($1, 'eng', '{"owner": {"kind": "system"}, "dirty": true}', '{"personalAccount": "off"}')`, ids["group"])
	exec(t, db, `INSERT INTO roles (id, name, metadata, spec) VALUES ($1, 'admin', '{"owner": {"kind": "system"}}', '{"rules": [{"kinds": ["*"], "verbs": ["*"]}], "enabled": true}'), ($2, 'key-reader', '{"owner": {"kind": "system"}}', '{"rules": [{"kinds": ["keys"], "verbs": ["get"]}]}')`,
		ids["admin-role"], ids["custom-role"])
	exec(t, db, `INSERT INTO role_bindings (id, name, role_id, scope_kind, scope_id, metadata, spec) VALUES
		($1, 'ops-admins', $2, 'system', NULL, '{"owner": {"kind": "system"}}', '{"enabled": true}'),
		($3, 'platform-readers', $4, 'team', $5, $6::jsonb, '{}')`,
		ids["global-binding"], ids["admin-role"], ids["team-binding"], ids["custom-role"], ids["team"], `{"owner": {"kind": "team", "id": "`+ids["team"]+`"}}`)
	exec(t, db, `INSERT INTO role_binding_subjects (binding_id, kind, subject_id, subject_user_id, position) VALUES ($1, 'user', $2, $2, 0)`, ids["global-binding"], ids["admin-user"])

	before44 := map[string]map[string]string{}
	beforeMoved := map[string]map[string]string{}
	for table, cols := range movedTables {
		before44[table] = tableRows(t, db, table, "name, display_name, metadata, spec")
		beforeMoved[table] = tableRows(t, db, table, cols)
	}
	before44["role_bindings_scope"] = tableRows(t, db, "role_bindings", "scope_kind, scope_id")

	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	for table, cols := range movedTables {
		if diff := cmp.Diff(beforeMoved[table], tableRows(t, db, table, cols)); diff != "" {
			t.Errorf("%s changed beside the owner (-44 +45):\n%s", table, diff)
		}
	}
	for _, tc := range []struct{ table, id, want string }{
		{"teams", ids["team"], "organization"},
		{"teams", ids["personal-team"], "user"},
		{"groups", ids["group"], "organization"},
		{"roles", ids["admin-role"], "system"},
		{"roles", ids["custom-role"], "organization"},
		{"role_bindings", ids["global-binding"], "organization"},
		{"role_bindings", ids["team-binding"], "team"},
		{"policy_bindings", pb.Meta.ID, "organization"},
		{"accounts", acct.Meta.ID, "organization"},
		{"policies", pol.Meta.ID, "system"}, // the boot decides, with the catalog tree in hand
	} {
		if got := ownerKind(t, db, tc.table, tc.id); got != tc.want {
			t.Errorf("%s %s owner = %q, want %q", tc.table, tc.id, got, tc.want)
		}
	}
	if got := tableRows(t, db, "role_bindings", "scope_kind, scope_id")[ids["global-binding"]]; !strings.Contains(got, `"scope_kind":"organization"`) {
		t.Errorf("global binding scope = %s, want organization", got)
	}

	// The boot moves the operator's policy; a user bound to the admin role globally still holds it at the global scope.
	cat, _, _, err := Bootstrap(ctx, BootstrapOptions{Pool: db})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := ownerKind(t, db, "policies", pol.Meta.ID); got != "organization" {
		t.Errorf("operator policy owner after boot = %q, want organization", got)
	}
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	bound := actor.WithActor(ctx, &actor.Actor{UserID: ids["admin-user"], Subjects: UserSubjects(ids["admin-user"], nil, nil)})
	for action, res := range map[string]authz.Resource{
		"teams.update":         {Kind: "team", ID: ids["team"], Owner: &meta.Global},
		"host-keys.update":     {Kind: "host-key", ID: meta.NewID(), Owner: &meta.Global},
		"role-bindings.create": {Kind: "role-binding", Owner: &meta.Global},
	} {
		if err := rbac.Authorize(bound, action, res); err != nil {
			t.Errorf("global admin binding after the upgrade: %s = %v, want allowed", action, err)
		}
	}
	admin := actor.WithActor(ctx, &actor.Actor{UserID: meta.NewID(), Roles: []string{user.RoleAdmin}})
	if err := rbac.Authorize(admin, "policies.update", authz.Resource{Kind: "policy", ID: pol.Meta.ID, Owner: &meta.Global}); err != nil {
		t.Errorf("admin on the moved policy = %v, want allowed", err)
	}

	if err := storage.MigrateTo(dsn, 44); err != nil {
		t.Fatalf("migrate down to 44: %v", err)
	}
	for table := range movedTables {
		if diff := cmp.Diff(before44[table], tableRows(t, db, table, "name, display_name, metadata, spec")); diff != "" {
			t.Errorf("%s after down differs from schema 44's rows (-before +after):\n%s", table, diff)
		}
	}
	if diff := cmp.Diff(before44["role_bindings_scope"], tableRows(t, db, "role_bindings", "scope_kind, scope_id")); diff != "" {
		t.Errorf("role binding scopes after down (-before +after):\n%s", diff)
	}
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
}

// The boot moves the operator's system rows and host keys, seals a stored value under the organization's data key, keeps what the catalog or the binary ships, and does nothing on a second pass.
func TestIntegration_OrganizationMove(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	_, _, stores, err := Bootstrap(ctx, BootstrapOptions{Pool: db, MasterKey: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatal(err)
	}
	acct := providerAccountAt(t, ctx, stores, hst.Meta.ID)
	upsert := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("upsert %s: %v", what, err)
		}
	}
	rule := []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyTokenBucket}}
	ops := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops", Owner: system}}
	detachedAt := time.Now().UTC()
	detached := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "dropped-by-catalog", Owner: system, CatalogDetachedAt: &detachedAt}}
	builtin := &ratelimit.RateLimit{Meta: meta.Metadata{ID: meta.NewID(), Name: "inference-api", Owner: system}, Spec: ratelimit.Spec{Rules: rule}}
	opsRL := &ratelimit.RateLimit{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-rpm", Owner: system}, Spec: ratelimit.Spec{Rules: rule}}
	bud := &budget.Budget{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-month", Owner: system}, Spec: budget.Spec{AmountUSD: "10", Period: budget.PeriodMonth, Mode: budget.ModeHard}}
	pl := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "ops-pool", Owner: system}, Spec: pool.Spec{HostID: hst.Meta.ID}}
	upsert("policy", stores.Policy.Upsert(ctx, ops))
	upsert("detached policy", stores.Policy.Upsert(ctx, detached))
	upsert("built-in rate limit", stores.RateLimit.Upsert(ctx, builtin))
	upsert("rate limit", stores.RateLimit.Upsert(ctx, opsRL))
	upsert("budget", stores.Budget.Upsert(ctx, bud))
	upsert("pool", stores.Pool.Upsert(ctx, pl))

	t.Setenv("ACME_ORG_KEY", "sk-env")
	storedKey := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: "stored", Owner: system},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, AccountID: acct, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, Value: "sk-stored"}}
	envKey := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: "env", Owner: system},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, AccountID: acct, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_ORG_KEY"}}}
	broken := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: "broken", Owner: system},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, AccountID: acct, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, Value: "sk-broken"}}
	for _, k := range []*hostkey.HostKey{storedKey, envKey, broken} {
		upsert("host key "+k.Meta.Name, stores.HostKey.Upsert(ctx, k))
	}
	// A value that no longer opens: the tag is flipped, the header and data key stay valid.
	exec(t, db, `UPDATE secret_values SET ciphertext = overlay(ciphertext placing decode(lpad(to_hex(get_byte(ciphertext, length(ciphertext) - 1) # 1), 2, '0'), 'hex') from length(ciphertext)) WHERE id = $1`, broken.Meta.ID)
	brokenBefore, err := gen.New(db).GetSecretValue(ctx, broken.Meta.ID)
	if err != nil {
		t.Fatal(err)
	}

	opts := BootstrapOptions{SystemRateLimits: []*ratelimit.RateLimit{{Meta: meta.Metadata{Name: "inference-api"}}}}
	res, err := moveToOrganization(ctx, stores, opts)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	want := &moveCounts{
		Moved:  map[string]int{"Policy": 1, "RateLimit": 1, "Budget": 1, "Pool": 1, "HostKey": 2},
		Kept:   map[string]int{"Policy": 1, "RateLimit": 1},
		Failed: map[string]int{"HostKey": 1},
	}
	if diff := cmp.Diff(want, res); diff != "" {
		t.Fatalf("first pass (-want +got):\n%s", diff)
	}
	for _, tc := range []struct{ table, id, want string }{
		{"policies", ops.Meta.ID, "organization"},
		{"policies", detached.Meta.ID, "system"},
		{"rate_limits", builtin.Meta.ID, "system"},
		{"rate_limits", opsRL.Meta.ID, "organization"},
		{"budgets", bud.Meta.ID, "organization"},
		{"pools", pl.Meta.ID, "organization"},
		{"secrets", storedKey.Meta.ID, "organization"},
		{"secrets", envKey.Meta.ID, "organization"},
		{"secrets", broken.Meta.ID, "system"},
	} {
		if got := ownerKind(t, db, tc.table, tc.id); got != tc.want {
			t.Errorf("%s %s owner = %q, want %q", tc.table, tc.id, got, tc.want)
		}
	}
	row, err := gen.New(db).GetSecretValue(ctx, storedKey.Meta.ID)
	if err != nil || row.Scope.String != stored.ScopeOrganization {
		t.Fatalf("stored value scope = %q (%v), want organization", row.Scope.String, err)
	}
	for _, k := range []*hostkey.HostKey{storedKey, envKey} {
		got, err := stores.HostKey.Get(ctx, k.Meta.ID)
		if err != nil || got == nil || got.Resolved == "" || got.Status.Unresolved != nil {
			t.Fatalf("%s after the move: %+v, %v; want it to resolve", k.Meta.Name, got, err)
		}
	}
	if got, _ := stores.HostKey.Get(ctx, storedKey.Meta.ID); got.Resolved != "sk-stored" {
		t.Errorf("stored key resolves to %q, want sk-stored", got.Resolved)
	}
	brokenAfter, err := gen.New(db).GetSecretValue(ctx, broken.Meta.ID)
	if err != nil || brokenAfter.Scope != brokenBefore.Scope || string(brokenAfter.Ciphertext) != string(brokenBefore.Ciphertext) {
		t.Errorf("a value that did not open was rewritten: scope %q→%q", brokenBefore.Scope.String, brokenAfter.Scope.String)
	}

	again, err := moveToOrganization(ctx, stores, opts)
	if err != nil {
		t.Fatalf("second move: %v", err)
	}
	if len(again.Moved) != 0 || again.Failed["HostKey"] != 1 {
		t.Errorf("second pass = %+v, want nothing moved and the broken key tried again", again)
	}
}

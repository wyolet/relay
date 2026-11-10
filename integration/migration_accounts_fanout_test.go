//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/catalogstore"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// at38 is a fresh database at 38, the schema before accounts were what policies bind.
type at38 struct {
	t    *testing.T
	ctx  context.Context
	dsn  string
	conn *pgxpool.Pool
}

func newAt38(t *testing.T) *at38 {
	t.Helper()
	dsn := storagetest.EmptyDB(t)
	if err := storagemod.MigrateTo(dsn, 38); err != nil {
		t.Fatalf("migrate to 38: %v", err)
	}
	return &at38{t: t, ctx: context.Background(), dsn: dsn, conn: testPool(t, dsn)}
}

func (d *at38) exec(what, sql string, args ...any) {
	d.t.Helper()
	if _, err := d.conn.Exec(d.ctx, sql, args...); err != nil {
		d.t.Fatalf("insert %s at 38: %v", what, err)
	}
}

func (d *at38) user(name string) string {
	id := meta.NewID()
	d.exec("user "+name, `INSERT INTO users (id, username) VALUES ($1, $2)`, id, name)
	return id
}

func (d *at38) team(name string) string {
	id := meta.NewID()
	d.exec("team "+name, `INSERT INTO teams (id, name, metadata, spec) VALUES ($1, $2, '{"owner":{"kind":"system"}}', '{}')`, id, name)
	return id
}

func (d *at38) project(name, teamID string) string {
	id := meta.NewID()
	d.exec("project "+name, `INSERT INTO projects (id, name, team_id, metadata, spec) VALUES ($1, $2, $3, $4, '{}')`, id, name, teamID, `{"owner":{"kind":"team","id":"`+teamID+`"}}`)
	return id
}

// roleBinding grants userID a role in a team or project, as the older schema stored it.
func (d *at38) roleBinding(name, scopeKind, scopeID, userID string) {
	roleID, id := meta.NewID(), meta.NewID()
	d.exec("role "+name, `INSERT INTO roles (id, name, metadata, spec) VALUES ($1, $2, '{"owner":{"kind":"system"}}', '{}')`, roleID, "role-"+name)
	d.exec("role binding "+name, `INSERT INTO role_bindings (id, name, role_id, scope_kind, scope_id, metadata, spec) VALUES ($1, $2, $3, $4, $5, $6, '{}')`,
		id, name, roleID, scopeKind, scopeID, `{"owner":{"kind":"`+scopeKind+`","id":"`+scopeID+`"}}`)
	d.exec("role binding subject", `INSERT INTO role_binding_subjects (binding_id, kind, subject_id, subject_user_id, position) VALUES ($1, 'user', $2, $2, 0)`, id, userID)
}

func (d *at38) policy(name, spec, models string) string {
	id := meta.NewID()
	d.exec("policy "+name, `INSERT INTO policies (id, name, metadata, spec, models) VALUES ($1, $2, '{"owner":{"kind":"system"}}', $3, $4)`, id, name, spec, models)
	return id
}

// userBinding binds policyID to userID from owner (system, team:<id> or project:<id>).
func (d *at38) userBinding(name, owner, policyID, userID string) string {
	id := meta.NewID()
	kind, ownerID, _ := strings.Cut(owner, ":")
	var projectID, teamID any
	switch kind {
	case "project":
		projectID = ownerID
	case "team":
		teamID = ownerID
	}
	d.exec("policy binding "+name, `INSERT INTO policy_bindings (id, name, project_id, team_id, policy_id, metadata, spec) VALUES ($1, $2, $3, $4, $5, $6, '{}')`,
		id, name, projectID, teamID, policyID, `{"owner":{"kind":"`+kind+`","id":"`+ownerID+`"}}`)
	scope := owner
	if kind == "system" {
		scope = ""
	}
	d.exec("policy binding subject", `INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_user_id, position, owner_scope) VALUES ($1, 'user', $2, $2, 0, $3)`, id, userID, scope)
	return id
}

// A user under a team's binding fans out to the team member account and each project member account the user has in that team; under the system's, to the personal account and every member account. Member accounts the upgrade makes serve tokens only, and each bound account carries the policy's payload-logging decision: off where it did not capture.
func TestMigrateUpFansUserBindingsOutToTheirAccounts(t *testing.T) {
	t.Parallel()
	d := newAt38(t)
	alice, bob := d.user("alice"), d.user("bob")
	ml, other := d.team("ml"), d.team("other")
	search, eval, lab := d.project("search", ml), d.project("eval", ml), d.project("lab", other)
	d.roleBinding("alice-search", "project", search, alice)
	d.roleBinding("alice-lab", "project", lab, alice)
	d.roleBinding("bob-eval", "project", eval, bob)
	teamWide := d.userBinding("ml-alice", "team:"+ml, d.policy("quiet", `{}`, `[]`), alice)
	everywhere := d.userBinding("bob-everywhere", "system:", d.policy("recorded", `{"payloadLoggingEnabled": true}`, `[]`), bob)

	if err := storagemod.MigrateUp(d.dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	q := gen.New(d.conn)
	all, err := account.NewStore(q).List(d.ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*account.Account{}
	for _, a := range all {
		byID[a.Meta.ID] = a
	}
	bindings := policybinding.NewStore(d.conn)
	for id, want := range map[string]struct {
		places  []string
		logging account.PayloadLogging
	}{
		teamWide:   {[]string{"member:project:" + search + ":" + alice, "member:team:" + ml + ":" + alice}, account.PayloadLoggingOff},
		everywhere: {[]string{"member:project:" + eval + ":" + bob, "personal:" + bob}, account.PayloadLoggingOn},
	} {
		b, err := bindings.Get(d.ctx, id)
		if err != nil || b == nil {
			t.Fatalf("binding %s: %v", id, err)
		}
		var places []string
		for _, accountID := range b.Spec.AccountIDs {
			a := byID[accountID]
			places = append(places, a.ManagedKey())
			if a.Spec.PayloadLogging != want.logging {
				t.Errorf("%s payloadLogging = %q, want %q", a.Meta.Name, a.Spec.PayloadLogging, want.logging)
			}
		}
		slices.Sort(places)
		if !slices.Equal(places, want.places) {
			t.Errorf("binding %s binds %v, want %v", b.Meta.Name, places, want.places)
		}
	}
	for _, a := range all {
		if a.ManagedKey() == "member:team:"+ml+":"+alice && (a.FollowsRoleBindings || a.TakesKeysOf(account.Holder{Kind: account.HolderUser, ID: alice})) {
			t.Errorf("the team member account the upgrade made = %+v, want it serving tokens only", a)
		}
	}
}

// A fan-out reaching an account another binding holds stops the upgrade, naming the account and both bindings, rather than picking one.
func TestMigrateUpStopsWhenAFanOutMeetsAnotherBinding(t *testing.T) {
	t.Parallel()
	d := newAt38(t)
	alice := d.user("alice")
	ml := d.team("ml")
	search := d.project("search", ml)
	d.roleBinding("alice-search", "project", search, alice)
	pol := d.policy("shared", `{}`, `[]`)
	d.userBinding("ml-alice", "team:"+ml, pol, alice)
	d.userBinding("search-alice", "project:"+search, pol, alice)

	err := storagemod.MigrateUp(d.dsn)
	if err == nil {
		t.Fatal("the upgrade went through with two bindings on one member account")
	}
	for _, want := range []string{"alice-project-search", "ml-alice", "search-alice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("upgrade error %q does not name %q", err, want)
		}
	}
}

// Dropping a pinned host from a bound policy's models stops the upgrade when the provider is served at another host the policy reaches: anywhere for a policy linking no pools, or a linked pool's host. A pin the policy's pools already confine upgrades.
func TestMigrateUpStopsWhenStrippingAHostWidens(t *testing.T) {
	t.Parallel()
	d := newAt38(t)
	system := `{"owner":{"kind":"system"}}`
	providerID, modelID, east, west := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	d.exec("provider", `INSERT INTO providers (id, name, metadata, spec) VALUES ($1, 'acme', $2, '{}')`, providerID, system)
	d.exec("model", `INSERT INTO models (id, name, metadata, spec) VALUES ($1, 'm', $2, '{}')`, modelID, `{"owner":{"kind":"provider","id":"`+providerID+`"}}`)
	for name, id := range map[string]string{"east": east, "west": west} {
		d.exec("host "+name, `INSERT INTO hosts (id, name, metadata, spec) VALUES ($1, $2, $3, '{"baseUrl":"https://api.example.com"}')`, id, name, system)
		d.exec("binding at "+name, `INSERT INTO host_bindings (id, name, model_id, host_id, metadata, spec) VALUES ($1, $2, $3, $4, $5, '{"adapter":"openai"}')`, meta.NewID(), "m-on-"+name, modelID, id, system)
	}
	poolID := meta.NewID()
	d.exec("pool", `INSERT INTO pools (id, name, host_id, metadata, spec) VALUES ($1, 'east-pool', $2, $3, '{}')`, poolID, east, system)
	alice := d.user("alice")
	ml := d.team("ml")
	search := d.project("search", ml)
	open := d.policy("open", `{}`, `["acme/m@east"]`)
	confined := d.policy("confined", `{}`, `["acme/m@east"]`)
	d.exec("policy pool", `INSERT INTO policy_pools (policy_id, pool_id, position) VALUES ($1, $2, 0)`, confined, poolID)
	limitID := meta.NewID()
	d.exec("rate limit", `INSERT INTO rate_limits (id, name, metadata, spec) VALUES ($1, 'east-rpm', $2, '{"rules":[]}')`, limitID, system)
	d.exec("scoped limit link", `INSERT INTO policy_rate_limits (policy_id, rate_limit_id, models, position) VALUES ($1, $2, '["acme/m@east"]', 0)`, confined, limitID)
	d.userBinding("open-alice", "project:"+search, open, alice)
	d.userBinding("confined-bob", "system:", confined, d.user("bob"))

	err := storagemod.MigrateUp(d.dsn)
	if err == nil || !strings.Contains(err.Error(), "policy open: acme/m@east") || strings.Contains(err.Error(), "confined") {
		t.Fatalf("upgrade = %v, want a stop naming policy open's pinned ref only", err)
	}

	d.exec("unbind", `DELETE FROM policy_bindings WHERE name = 'open-alice'`)
	if err := storagemod.ForceVersion(d.dsn, 38); err != nil {
		t.Fatalf("clear the failed step: %v", err)
	}
	if err := storagemod.MigrateUp(d.dsn); err != nil {
		t.Fatalf("upgrade with the pin confined by its pool: %v", err)
	}
	var scope string
	if err := d.conn.QueryRow(d.ctx, `SELECT models::text FROM policy_rate_limits WHERE policy_id = $1`, confined).Scan(&scope); err != nil || scope != `["acme/m"]` {
		t.Fatalf("the limit link's scope = %s (err %v), want its host dropped", scope, err)
	}
}

// Rolling back to 38 maps every account of a user back to the one user subject; where several land in one owner scope, the oldest binding keeps the user and each other is dropped.
func TestMigrateDownKeepsOneBindingPerUserAndScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := st.Pool()
	q := gen.New(db)
	alice := &user.User{ID: meta.NewID(), Username: "alice"}
	mustUpsert(t, user.NewStore(db).Upsert(ctx, alice), "user")
	accounts := account.NewStore(q)
	personal, err := accounts.EnsureManaged(ctx, account.Personal(alice.ID, alice.Username))
	if err != nil {
		t.Fatal(err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "shared", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	mustUpsert(t, catalogstore.NewPolicyStore(db).Upsert(ctx, pol), "policy")
	bindings := policybinding.NewStore(db)
	older := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "older", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{personal}}}
	mustUpsert(t, bindings.Upsert(ctx, older), "older binding")
	// A second system binding on another account of alice's maps back to the same user subject at 38.
	ml := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	mustUpsert(t, team.NewStore(db).Upsert(ctx, ml), "team")
	member, err := accounts.EnsureManaged(ctx, account.Member(alice.ID, alice.Username, meta.Owner{Kind: meta.OwnerTeam, ID: ml.Meta.ID}, ml.Meta.Name, ""))
	if err != nil {
		t.Fatalf("member account: %v", err)
	}
	newer := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "newer", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{member}}}
	mustUpsert(t, bindings.Upsert(ctx, newer), "newer binding")
	st.Close()

	conn := testPool(t, dsn)
	if err := storagemod.MigrateTo(dsn, 38); err != nil {
		t.Fatalf("migrate down to 38: %v", err)
	}
	count := func(bindingID string) int {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM policy_binding_subjects WHERE binding_id = $1 AND kind = 'user' AND subject_id = $2`, bindingID, alice.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(older.Meta.ID) != 1 || count(newer.Meta.ID) != 0 {
		t.Fatalf("alice at 38: older binding %d, newer %d; want the older binding alone to keep her", count(older.Meta.ID), count(newer.Meta.ID))
	}
}

//go:build integration

package integration_test

import (
	"context"
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
	"github.com/wyolet/relay/app/team"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

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

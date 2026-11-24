//go:build integration

// store_integration_test.go covers the ServiceAccount store's owners and migrations 000047 and 000048 against a real Postgres. Run with: make test-integration.

package serviceaccount_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/meta"
)

// A user's service account is stored with the user as its owner; deleting the user deletes it, its account and its keys, and leaves a project's service account alone.
func TestIntegration_UserServiceAccountGoesWithItsUser(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	x := newTenancy(t, pool)
	users := user.NewStore(pool)
	ada := &user.User{ID: meta.NewID(), Username: "ada"}
	if err := users.Upsert(ctx, ada); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "ada-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: ada.ID}}}
	inProject := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: x.project.Meta.ID}}
	for _, sa := range []*serviceaccount.ServiceAccount{bot, inProject} {
		if err := x.accounts.Upsert(ctx, sa); err != nil {
			t.Fatalf("upsert %s: %v", sa.Meta.Name, err)
		}
	}
	got, err := x.accounts.Get(ctx, bot.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("get = %v, %v", got, err)
	}
	if diff := cmp.Diff(bot, got, ignoreStoreFields); diff != "" || got.Spec.UserID != ada.ID {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
	q := gen.New(pool)
	acctID, err := account.NewStore(q).EnsureManaged(ctx, account.ForServiceAccount(bot.Meta.ID, bot.Meta.Name, "", bot.Meta.Owner))
	if err != nil {
		t.Fatalf("ensure the bot's account: %v", err)
	}
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "ada-bot-key", Owner: bot.Meta.Owner},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: bot.Meta.ID}, AccountID: acctID, KeyHash: strings.Repeat("a", 64), Prefix: "sk-ada-bot"}}
	if err := key.NewStore(q).Upsert(ctx, k); err != nil {
		t.Fatalf("upsert key: %v", err)
	}

	if err := users.Delete(ctx, ada.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got, err := x.accounts.Get(ctx, bot.Meta.ID); err != nil || got != nil {
		t.Errorf("the user's service account after the user's delete = %v, %v", got, err)
	}
	if got, err := account.NewStore(q).Get(ctx, acctID); err != nil || got != nil {
		t.Errorf("the bot's account after the user's delete = %v, %v", got, err)
	}
	if got, err := key.NewStore(q).Get(ctx, k.Meta.ID); err != nil || got != nil {
		t.Errorf("the bot's key after the user's delete = %v, %v", got, err)
	}
	if got, err := x.accounts.Get(ctx, inProject.Meta.ID); err != nil || got == nil {
		t.Errorf("a project's service account after a user's delete = %v, %v", got, err)
	}
}

// Migration 000048's down refuses while a user owns a service account, and once none does drops the user column; up again restores user owners.
func TestIntegration_ServiceAccountUserOwnerMigrationDown(t *testing.T) {
	ctx := context.Background()
	dsn := storagetest.DB(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	x := newTenancy(t, pool)
	users := user.NewStore(pool)
	ada := &user.User{ID: meta.NewID(), Username: "ada"}
	if err := users.Upsert(ctx, ada); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "ada-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: ada.ID}}}
	inTeam := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: x.team.Meta.ID}}}
	for _, sa := range []*serviceaccount.ServiceAccount{bot, inTeam} {
		if err := x.accounts.Upsert(ctx, sa); err != nil {
			t.Fatalf("upsert %s: %v", sa.Meta.Name, err)
		}
	}
	pool.Reset()

	if err := storage.MigrateTo(dsn, 47); err == nil || !strings.Contains(err.Error(), "owned by a user") {
		t.Fatalf("migrate down to 47 = %v, want 000048's refusal", err)
	}
	if err := x.accounts.Delete(ctx, bot.Meta.ID); err != nil {
		t.Fatalf("delete the user's service account: %v", err)
	}
	pool.Reset()
	if err := storage.MigrateTo(dsn, 47); err != nil {
		t.Fatalf("migrate down to 47: %v", err)
	}
	var userColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'service_accounts' AND column_name = 'user_id'`).Scan(&userColumns); err != nil {
		t.Fatal(err)
	}
	if userColumns != 0 {
		t.Fatalf("user_id columns after the down = %d, want none", userColumns)
	}

	pool.Reset()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if got, err := x.accounts.Get(ctx, inTeam.Meta.ID); err != nil || got == nil || got.Meta.Owner != inTeam.Meta.Owner {
		t.Fatalf("team service account after down and up = %+v, %v", got, err)
	}
	if err := x.accounts.Upsert(ctx, bot); err != nil {
		t.Fatalf("user service account after down and up: %v", err)
	}
}

type tenancy struct {
	teams    *team.Store
	projects *project.Store
	accounts *serviceaccount.Store
	team     *team.Team
	project  *project.Project
}

func newTenancy(t *testing.T, pool *pgxpool.Pool) tenancy {
	t.Helper()
	ctx := context.Background()
	x := tenancy{teams: team.NewStore(pool), projects: project.NewStore(pool), accounts: serviceaccount.NewStore(pool)}
	x.team = &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Global}}
	if err := x.teams.Upsert(ctx, x.team); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	x.project = &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "search"}, Spec: project.Spec{TeamID: x.team.Meta.ID}}
	x.project.StampOwner()
	if err := x.projects.Upsert(ctx, x.project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	return x
}

var ignoreStoreFields = cmpopts.IgnoreFields(meta.Metadata{}, "ResourceVersion", "CreatedAt", "UpdatedAt")

// A service account is stored with a project or a team as its owner and reads back the same; the team that owns one is not deleted under it.
func TestIntegration_ServiceAccountOwners(t *testing.T) {
	ctx := context.Background()
	x := newTenancy(t, storagetest.Pool(t))
	inProject := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: x.project.Meta.ID}}
	inTeam := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: x.team.Meta.ID}}}
	for _, sa := range []*serviceaccount.ServiceAccount{inProject, inTeam} {
		if err := x.accounts.Upsert(ctx, sa); err != nil {
			t.Fatalf("upsert %s: %v", sa.Meta.Name, err)
		}
		got, err := x.accounts.Get(ctx, sa.Meta.ID)
		if err != nil || got == nil {
			t.Fatalf("get %s = %v, %v", sa.Meta.Name, got, err)
		}
		if diff := cmp.Diff(sa, got, ignoreStoreFields); diff != "" {
			t.Errorf("%s round trip (-want +got):\n%s", sa.Meta.Name, diff)
		}
	}
	if inProject.Meta.Owner != (meta.Owner{Kind: meta.OwnerProject, ID: x.project.Meta.ID}) || inTeam.Spec.TeamID != x.team.Meta.ID {
		t.Fatalf("stamped owners = %+v / %+v %+v", inProject.Meta.Owner, inTeam.Meta.Owner, inTeam.Spec)
	}

	both := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "both", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: x.team.Meta.ID}}, Spec: serviceaccount.Spec{ProjectID: x.project.Meta.ID}}
	if err := x.accounts.Upsert(ctx, both); err == nil {
		t.Error("stored a service account with a project and a team")
	}
	missingTeam := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "stray", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}}}
	if err := x.accounts.Upsert(ctx, missingTeam); err == nil {
		t.Error("stored a service account of a missing team")
	}

	if err := x.teams.Delete(ctx, x.team.Meta.ID); err == nil {
		t.Fatal("deleted a team that owns a service account")
	}
	if got, _ := x.projects.Get(ctx, x.project.Meta.ID); got == nil {
		t.Fatal("a refused team delete took the team's project")
	}
	if err := x.accounts.Delete(ctx, inTeam.Meta.ID); err != nil {
		t.Fatalf("delete team service account: %v", err)
	}
	if err := x.teams.Delete(ctx, x.team.Meta.ID); err != nil {
		t.Fatalf("team delete once it owns no service account: %v", err)
	}
	if got, _ := x.accounts.Get(ctx, inProject.Meta.ID); got != nil {
		t.Error("team delete left a service account of its project")
	}
}

// Migration 000047's down refuses while a team owns a service account, and once none does it restores the project-only schema; up again restores team owners.
func TestIntegration_ServiceAccountTeamOwnerMigrationDown(t *testing.T) {
	ctx := context.Background()
	dsn := storagetest.DB(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	x := newTenancy(t, pool)
	inProject := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: x.project.Meta.ID}}
	inTeam := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: x.team.Meta.ID}}}
	for _, sa := range []*serviceaccount.ServiceAccount{inProject, inTeam} {
		if err := x.accounts.Upsert(ctx, sa); err != nil {
			t.Fatalf("upsert %s: %v", sa.Meta.Name, err)
		}
	}
	pool.Reset()

	if err := storage.MigrateTo(dsn, 46); err == nil {
		t.Fatal("migrated down to 46 while a team owns a service account")
	}
	if err := storage.ForceVersion(dsn, 47); err != nil {
		t.Fatalf("clear the failed down's dirty flag: %v", err)
	}
	if err := x.accounts.Delete(ctx, inTeam.Meta.ID); err != nil {
		t.Fatalf("delete team service account: %v", err)
	}
	pool.Reset()
	if err := storage.MigrateTo(dsn, 46); err != nil {
		t.Fatalf("migrate down to 46: %v", err)
	}
	var nullable string
	var teamColumns int
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'service_accounts' AND column_name = 'project_id'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'service_accounts' AND column_name = 'team_id'`).Scan(&teamColumns); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" || teamColumns != 0 {
		t.Fatalf("after the down: project_id nullable %s, team_id columns %d; want NOT NULL and none", nullable, teamColumns)
	}

	pool.Reset()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if got, err := x.accounts.Get(ctx, inProject.Meta.ID); err != nil || got == nil || got.Meta.Owner != inProject.Meta.Owner {
		t.Fatalf("project service account after down and up = %+v, %v", got, err)
	}
	if err := x.accounts.Upsert(ctx, inTeam); err != nil {
		t.Fatalf("team service account after down and up: %v", err)
	}
}

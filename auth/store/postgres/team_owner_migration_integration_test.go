//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

// forceModule records version as the module's schema version and clears the dirty flag a failed migration leaves.
func forceModule(t *testing.T, dsn string, version int) {
	t.Helper()
	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	target, err := migrateURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	must(t, m.Force(version))
}

// serviceAccountOwnerColumns is how service_accounts reads at one schema version: whether project_id is nullable and whether team_id exists.
func serviceAccountOwnerColumns(t *testing.T, dsn string) (projectNullable string, teamColumns int) {
	t.Helper()
	pool := openPool(t, dsn)
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'service_accounts' AND column_name = 'project_id'`).Scan(&projectNullable); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'service_accounts' AND column_name = 'team_id'`).Scan(&teamColumns); err != nil {
		t.Fatal(err)
	}
	return projectNullable, teamColumns
}

// On the module's own schema 000006's down refuses while a team owns a service account, and once none does restores the project-only table; up again brings team owners back. On a product's schema the down leaves the columns to the product's own migrations.
func TestServiceAccountTeamOwnerDownMigration(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			migrateModule(t, dsn, embeddedVersion(t))
			stores := New(openPool(t, dsn))
			tm := &directory.Team{Meta: directory.Metadata{ID: directory.NewID(), Name: "core", Owner: authz.Owner(directory.Global)}}
			must(t, stores.Teams.Upsert(ctx, tm))
			bot := &directory.ServiceAccount{Meta: directory.Metadata{ID: directory.NewID(), Name: "bot"}, Spec: directory.ServiceAccountSpec{TeamID: tm.Meta.ID}}
			must(t, stores.ServiceAccounts.Upsert(ctx, bot))

			if withProduct {
				migrateModule(t, dsn, 5)
				if nullable, teamColumns := serviceAccountOwnerColumns(t, dsn); nullable != "YES" || teamColumns != 1 {
					t.Fatalf("module down over the product's schema: project_id nullable %s, team_id columns %d; want the product's columns kept", nullable, teamColumns)
				}
				return
			}

			if err := MigrateDown(dsn, 5); err == nil {
				t.Fatal("migrated down to 5 while a team owns a service account")
			}
			forceModule(t, dsn, 6)
			must(t, stores.ServiceAccounts.Delete(ctx, bot.Meta.ID))
			migrateModule(t, dsn, 5)
			if nullable, teamColumns := serviceAccountOwnerColumns(t, dsn); nullable != "NO" || teamColumns != 0 {
				t.Fatalf("after the down: project_id nullable %s, team_id columns %d; want NOT NULL and none", nullable, teamColumns)
			}
			migrateModule(t, dsn, 6)
			must(t, New(openPool(t, dsn)).ServiceAccounts.Upsert(ctx, bot))
		})
	}
}

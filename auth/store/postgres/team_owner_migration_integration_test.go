//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

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
			migrateModule(t, dsn, embeddedVersion(t))
			must(t, New(openPool(t, dsn)).ServiceAccounts.Upsert(ctx, bot))
		})
	}
}

// On the module's own schema a down that counts the rows it refuses on holds the table: a row a transaction inserts while it counts is refused, not left to fail the down's ALTERs.
func TestServiceAccountOwnerDownsHoldTheTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      uint
		col     string
		refusal string
	}{
		{"000007, a user owner", 7, "user_id", "auth migration 000007 down: 1 service accounts are owned by a user; delete them first"},
		{"000006, a team owner", 6, "team_id", "auth migration 000006 down: 1 service accounts are owned by a team; delete them first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, false)
			migrateModule(t, dsn, tc.at)
			stores := New(openPool(t, dsn))
			tm := &directory.Team{Meta: directory.Metadata{ID: directory.NewID(), Name: "core", Owner: authz.Owner(directory.Global)}}
			must(t, stores.Teams.Upsert(ctx, tm))
			ada := &directory.User{ID: directory.NewID(), Username: "ada"}
			must(t, stores.Users.Upsert(ctx, ada))
			owner := map[string]string{"user_id": ada.ID, "team_id": tm.Meta.ID}[tc.col]

			pool := openPool(t, dsn)
			tx, err := pool.Begin(ctx)
			must(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = tx.Exec(ctx, `INSERT INTO service_accounts (id, name, metadata, spec, `+tc.col+`) VALUES ($1, 'racer', '{}', '{}', $2)`, directory.NewID(), owner)
			must(t, err)
			done := make(chan error, 1)
			go func() { done <- MigrateDown(dsn, tc.at-1) }()
			for i := 0; ; i++ {
				var waiting int
				must(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting))
				if waiting > 0 {
					break
				}
				if i == 200 {
					t.Fatal("the down migration never waited on a lock")
				}
				time.Sleep(25 * time.Millisecond)
			}
			must(t, tx.Commit(ctx))
			if err := <-done; err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("down to %d = %v, want the refusal naming the row inserted meanwhile", tc.at-1, err)
			}
		})
	}
}

// userOwnerColumns counts service_accounts' user_id columns.
func userOwnerColumns(t *testing.T, dsn string) (n int) {
	t.Helper()
	if err := openPool(t, dsn).QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'service_accounts' AND column_name = 'user_id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// On the module's own schema 000007's down refuses while a user owns a service account, and once none does drops the user column; up again brings user owners back, and deleting the user deletes their service account. On a product's schema the down leaves the column to the product's own migrations.
func TestServiceAccountUserOwnerDownMigration(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			migrateModule(t, dsn, embeddedVersion(t))
			stores := New(openPool(t, dsn))
			ada := &directory.User{ID: directory.NewID(), Username: "ada"}
			must(t, stores.Users.Upsert(ctx, ada))
			bot := &directory.ServiceAccount{Meta: directory.Metadata{ID: directory.NewID(), Name: "ada-bot"}, Spec: directory.ServiceAccountSpec{UserID: ada.ID}}
			must(t, stores.ServiceAccounts.Upsert(ctx, bot))

			if withProduct {
				migrateModule(t, dsn, 6)
				if n := userOwnerColumns(t, dsn); n != 1 {
					t.Fatalf("module down over the product's schema: user_id columns %d; want the product's column kept", n)
				}
				return
			}

			if err := MigrateDown(dsn, 6); err == nil {
				t.Fatal("migrated down to 6 while a user owns a service account")
			}
			forceModule(t, dsn, 7)
			must(t, stores.ServiceAccounts.Delete(ctx, bot.Meta.ID))
			migrateModule(t, dsn, 6)
			if n := userOwnerColumns(t, dsn); n != 0 {
				t.Fatalf("after the down: user_id columns %d; want none", n)
			}
			migrateModule(t, dsn, embeddedVersion(t))
			stores = New(openPool(t, dsn))
			must(t, stores.ServiceAccounts.Upsert(ctx, bot))
			must(t, stores.Users.Delete(ctx, ada.ID))
			if got, err := stores.ServiceAccounts.Get(ctx, bot.Meta.ID); err != nil || got != nil {
				t.Fatalf("the user's service account after the user's delete = %+v, %v; want it gone", got, err)
			}
		})
	}
}

//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/wyolet/relay/auth/directory"
)

// The identities migration expands the schema without contracting it: users.oidc_subject keeps the primary identity, so a binary from before it reads and writes accounts as it did. These tests run it on the product's schema and on the module's own.

// migrateModule moves the module's schema to version, down or up.
func migrateModule(t *testing.T, dsn string, version uint) {
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
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate module to %d: %v", version, err)
	}
}

func embeddedVersion(t *testing.T) uint {
	t.Helper()
	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	v, err := lastVersion(src)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// atFirstMigration returns a database at the module's version 1, over the product's schema or on its own.
func atFirstMigration(t *testing.T, withProduct bool) string {
	t.Helper()
	dsn := emptyDB(t)
	if withProduct {
		migrateProduct(t, dsn)
	}
	migrateModule(t, dsn, 1)
	return dsn
}

var schemas = map[string]bool{"product schema": true, "module schema": false}

// oldRead is the lookup a binary from before the identities table runs at sign-in.
func oldRead(t *testing.T, dsn, oidcSubject string) string {
	t.Helper()
	var id string
	err := openPool(t, dsn).QueryRow(context.Background(), "SELECT id FROM users WHERE oidc_subject = $1", oidcSubject).Scan(&id)
	if err != nil && !notFound(err) {
		t.Fatal(err)
	}
	return id
}

func identityRows(t *testing.T, dsn string) []string {
	t.Helper()
	rows, err := openPool(t, dsn).Query(context.Background(), `
SELECT i.user_id || ' ' || i.issuer || ' ' || i.subject || ' ' || coalesce(i.external_id, '-') || ' ' || (i.created_at = u.created_at)
  FROM identities i JOIN users u ON u.id = i.user_id ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestIdentitiesBackfill(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			pool := openPool(t, dsn)
			if _, err := pool.Exec(ctx, `
INSERT INTO users (id, username, oidc_subject) VALUES
    ('u-1', 'ada', 'https://idp.example.com|sub-1'),
    ('u-2', 'bob', 'https://idp.example.com|tenant|sub-2'),
    ('u-3', 'eve', 'bare-subject'),
    ('u-4', 'joe', NULL)`); err != nil {
				t.Fatal(err)
			}
			migrateModule(t, dsn, 2)

			want := []string{"u-1 https://idp.example.com sub-1 - true", "u-2 https://idp.example.com tenant|sub-2 - true"}
			if got := identityRows(t, dsn); !slices.Equal(got, want) {
				t.Fatalf("identities = %q, want %q", got, want)
			}
			users := New(pool).Users
			for id, idp := range map[string]directory.Identity{
				"u-1": {Issuer: "https://idp.example.com", Subject: "sub-1"},
				"u-2": {Issuer: "https://idp.example.com", Subject: "tenant|sub-2"},
			} {
				u, err := users.ByIdentity(ctx, idp)
				if err != nil || u == nil || u.ID != id || u.Identity != idp || len(u.Identities) != 1 {
					t.Fatalf("sign-in as %+v = %+v, %v; want %s", idp, u, err, id)
				}
			}
			bare, err := users.Get(ctx, "u-3")
			if err != nil || bare.Identity != (directory.Identity{Subject: "bare-subject"}) || bare.Identities != nil {
				t.Fatalf("a value without a separator read back as %+v, %v", bare, err)
			}
		})
	}
}

// A binary from before the identities table keeps working on accounts the newer binary writes, and the newer binary finds accounts the older one writes.
func TestIdentitiesSurviveRollback(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			migrateModule(t, dsn, 2)
			pool := openPool(t, dsn)
			d, err := directory.New(directory.Config{Stores: New(pool), Grants: allowAll{}})
			if err != nil {
				t.Fatal(err)
			}
			users := d.Stores().Users
			a := directory.Identity{Issuer: "https://idp.example.com", Subject: "a"}
			b := directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "b"}}

			ada := &directory.User{Username: "ada", Identity: a}
			must(t, d.CreateUser(ctx, nil, ada))
			if got := oldRead(t, dsn, "https://idp.example.com|a"); got != ada.ID {
				t.Fatalf("old read of a created user = %q, want %q", got, ada.ID)
			}
			must(t, d.AddIdentity(ctx, ada.ID, b))
			must(t, d.RemoveIdentity(ctx, ada.ID, a))
			if got := oldRead(t, dsn, "https://other.example.com|b"); got != ada.ID {
				t.Fatalf("old read after the primary moved = %q, want %q", got, ada.ID)
			}
			if got := oldRead(t, dsn, "https://idp.example.com|a"); got != "" {
				t.Fatalf("old read of a removed identity = %q", got)
			}

			bob := &directory.User{Username: "bob"}
			must(t, d.CreateUser(ctx, nil, bob))
			must(t, d.AddIdentity(ctx, bob.ID, directory.UserIdentity{Identity: a}))
			if got := oldRead(t, dsn, "https://idp.example.com|a"); got != bob.ID {
				t.Fatalf("old read of a first added identity = %q, want %q", got, bob.ID)
			}
			must(t, d.AddIdentity(ctx, bob.ID, directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com", Subject: "c"}}))
			cleared, err := users.Get(ctx, bob.ID)
			must(t, err)
			cleared.Identity = directory.Identity{}
			must(t, d.UpdateUser(ctx, nil, cleared))
			if got := oldRead(t, dsn, "https://other.example.com|c"); got != bob.ID {
				t.Fatalf("old read after a cleared primary was promoted = %q, want %q", got, bob.ID)
			}

			// The older binary's own writes: a sign-up, a changed identity, a delete.
			if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, oidc_subject) VALUES ('old-1', 'eve', 'https://idp.example.com|eve')`); err != nil {
				t.Fatal(err)
			}
			eve := directory.Identity{Issuer: "https://idp.example.com", Subject: "eve"}
			if u, err := users.ByIdentity(ctx, eve); err != nil || u == nil || u.ID != "old-1" {
				t.Fatalf("sign-in as an account the old binary created = %+v, %v", u, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE users SET oidc_subject = 'https://idp.example.com|eve-2' WHERE id = 'old-1'`); err != nil {
				t.Fatal(err)
			}
			if u, err := users.ByIdentity(ctx, eve); err != nil || u != nil {
				t.Fatalf("sign-in by an identity the old binary replaced = %+v, %v", u, err)
			}
			if u, err := users.ByIdentity(ctx, directory.Identity{Issuer: "https://idp.example.com", Subject: "eve-2"}); err != nil || u == nil || u.ID != "old-1" {
				t.Fatalf("sign-in by the replacing identity = %+v, %v", u, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE users SET oidc_subject = 'https://other.example.com|c' WHERE id = 'old-1'`); err == nil {
				t.Fatal("the old binary took another account's second identity")
			}
			if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = 'old-1'`); err != nil {
				t.Fatal(err)
			}
			for _, r := range identityRows(t, dsn) {
				if strings.HasPrefix(r, "old-1 ") {
					t.Fatalf("identity outlived its account: %s", r)
				}
			}
		})
	}
}

// The down migration removes what the identities migration added and nothing else; the accounts and their primary identities stay.
func TestIdentitiesDownMigration(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			tables := allTables(t, dsn)
			before := schema(t, dsn, tables, true)

			migrateModule(t, dsn, 2)
			if got := allTables(t, dsn); !slices.Contains(got, "identities") {
				t.Fatalf("tables after the up migration: %v", got)
			}
			d, err := directory.New(directory.Config{Stores: New(openPool(t, dsn)), Grants: allowAll{}})
			if err != nil {
				t.Fatal(err)
			}
			ada := &directory.User{Username: "ada", Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "a"},
				External: directory.External{Source: "scim-1", ID: "e-1"}}
			must(t, d.CreateUser(ctx, nil, ada))
			must(t, d.CreateGroup(ctx, nil, &directory.Group{Meta: directory.Metadata{Name: "ops"}, Spec: directory.GroupSpec{External: ada.External}}))

			migrateModule(t, dsn, 1)
			if got := allTables(t, dsn); !slices.Equal(got, tables) {
				t.Fatalf("tables after the down migration:\n got  %v\n want %v", got, tables)
			}
			diff(t, "schema after the down migration", schema(t, dsn, tables, true), before)
			if got := oldRead(t, dsn, "https://idp.example.com|a"); got != ada.ID {
				t.Fatalf("account after the down migration = %q, want %q", got, ada.ID)
			}
			var groups int
			if err := openPool(t, dsn).QueryRow(ctx, "SELECT count(*) FROM groups").Scan(&groups); err != nil || groups != 1 {
				t.Fatalf("groups after the down migration = %d, %v", groups, err)
			}
		})
	}
}

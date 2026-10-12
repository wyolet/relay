//go:build integration

package postgres

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/directory"
)

// Accounts written before 000005 with a slashed issuer, as a binary that stores its configured issuer verbatim leaves them, are found by either form of the issuer after it, and still by the old exact lookup.
func TestCanonicalIssuerMigration(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			pool := openPool(t, dsn)
			if _, err := pool.Exec(ctx, `
INSERT INTO users (id, username, oidc_subject, created_at) VALUES
    ('u-1', 'ada', 'https://idp.example.com/|sub-1', now() - interval '3 days'),
    ('u-2', 'bob', 'https://idp.example.com|sub-2',  now() - interval '3 days'),
    ('u-3', 'eve', 'https://idp.example.com/|sub-3', now() - interval '2 days'),
    ('u-4', 'joe', 'https://idp.example.com|sub-3',  now() - interval '1 day')`); err != nil {
				t.Fatal(err)
			}
			migrateModule(t, dsn, 4)
			want := []string{
				"u-1 https://idp.example.com/ sub-1 - true", "u-2 https://idp.example.com sub-2 - true",
				"u-3 https://idp.example.com/ sub-3 - true", "u-4 https://idp.example.com sub-3 - true",
			}
			if got := identityRows(t, dsn); !slices.Equal(got, want) {
				t.Fatalf("identities before = %q, want %q", got, want)
			}

			migrateModule(t, dsn, 5)
			// Of eve and joe, who differ only by the slash, the older keeps the identity.
			want = []string{"u-1 https://idp.example.com sub-1 - true", "u-2 https://idp.example.com sub-2 - true", "u-3 https://idp.example.com sub-3 - true"}
			if got := identityRows(t, dsn); !slices.Equal(got, want) {
				t.Fatalf("identities after = %q, want %q", got, want)
			}
			for subject, id := range map[string]string{"https://idp.example.com/|sub-1": "u-1", "https://idp.example.com|sub-2": "u-2", "https://idp.example.com|sub-3": "u-4"} {
				if got := oldRead(t, dsn, subject); got != id {
					t.Fatalf("old read of %s = %q, want %q", subject, got, id)
				}
			}
			users := New(pool).Users
			for _, issuer := range []string{"https://idp.example.com", "https://idp.example.com/"} {
				u, err := users.ByIdentity(ctx, directory.Identity{Issuer: issuer, Subject: "sub-1"})
				if err != nil || u == nil || u.ID != "u-1" || u.Identity.Issuer != "https://idp.example.com/" {
					t.Fatalf("sign-in through %s = %+v, %v; want u-1 with its primary as written", issuer, u, err)
				}
			}

			// A binary that only knows oidc_subject: its sign-up is mirrored canonically, and a change of the slash alone keeps the row.
			if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, oidc_subject) VALUES ('old-1', 'kim', 'https://idp.example.com/|kim')`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE identities SET external_id = 'obj-kim' WHERE user_id = 'old-1'`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE users SET oidc_subject = 'https://idp.example.com|kim' WHERE id = 'old-1'`); err != nil {
				t.Fatal(err)
			}
			if got := identityRows(t, dsn); !slices.Contains(got, "old-1 https://idp.example.com kim obj-kim true") {
				t.Fatalf("identities after a slash-only change = %q", got)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, oidc_subject) VALUES ('old-2', 'lee', 'https://idp.example.com/|kim')`); err == nil {
				t.Fatal("an account took an identity another holds by adding a slash")
			}
		})
	}
}

// The down migration gives each primary identity back its issuer as oidc_subject spells it and restores the verbatim mirror.
func TestCanonicalIssuerDownMigration(t *testing.T) {
	for name, withProduct := range schemas {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dsn := atFirstMigration(t, withProduct)
			pool := openPool(t, dsn)
			if _, err := pool.Exec(ctx, `
INSERT INTO users (id, username, oidc_subject) VALUES
    ('u-1', 'ada', 'https://idp.example.com/|sub-1'),
    ('u-2', 'bob', 'https://idp.example.com|sub-2')`); err != nil {
				t.Fatal(err)
			}
			migrateModule(t, dsn, embeddedVersion(t))
			must(t, New(pool).Users.AddIdentity(ctx, "u-1", directory.UserIdentity{Identity: directory.Identity{Issuer: "https://other.example.com/", Subject: "o-1"}}))

			migrateModule(t, dsn, 4)
			want := []string{
				"u-1 https://idp.example.com/ sub-1 - true", "u-1 https://other.example.com o-1 - false", "u-2 https://idp.example.com sub-2 - true",
			}
			if got := identityRows(t, dsn); !slices.Equal(got, want) {
				t.Fatalf("identities after the down migration = %q, want %q", got, want)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, oidc_subject) VALUES ('u-3', 'eve', 'https://idp.example.com/|sub-3')`); err != nil {
				t.Fatal(err)
			}
			if got := identityRows(t, dsn); !slices.Contains(got, "u-3 https://idp.example.com/ sub-3 - true") {
				t.Fatalf("mirror after the down migration = %q", got)
			}
			if got := oldRead(t, dsn, "https://idp.example.com/|sub-1"); got != "u-1" {
				t.Fatalf("old read after the down migration = %q", got)
			}
		})
	}
}

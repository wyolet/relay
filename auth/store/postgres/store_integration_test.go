//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/storetest"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) directory.Stores { return New(openPool(t, migratedDB(t))) })
}

func TestMigrateIsIdempotent(t *testing.T) {
	dsn := migratedDB(t)
	if err := Migrate(dsn); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var version int64
	var dirty bool
	if err := openPool(t, dsn).QueryRow(context.Background(),
		"SELECT version, dirty FROM "+MigrationsTable).Scan(&version, &dirty); err != nil {
		t.Fatal(err)
	}
	if version != 1 || dirty {
		t.Fatalf("recorded version %d dirty=%v, want 1 clean", version, dirty)
	}
}

func TestMigrateRejectsOtherSchemes(t *testing.T) {
	if err := Migrate("mysql://localhost/x"); err == nil {
		t.Fatal("a non-postgres dsn was accepted")
	}
}

// TestStoresShareCallerTransaction checks a write made through New(tx) is undone with the caller's transaction, including the multi-table group write.
func TestStoresShareCallerTransaction(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t, migratedDB(t))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := New(tx)
	u := &directory.User{ID: directory.NewID(), Username: "ada"}
	g := &directory.Group{Meta: directory.Metadata{ID: directory.NewID(), Name: "ops", Owner: authz.Owner{Kind: "system"}},
		Spec: directory.GroupSpec{MemberIDs: []string{u.ID}}}
	if err := s.Users.Upsert(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Groups.Upsert(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	after := New(pool)
	if got, err := after.Groups.Get(ctx, g.Meta.ID); err != nil || got != nil {
		t.Fatalf("group survived the rollback: %v, %v", got, err)
	}
}

type allowAll struct{}

func (allowAll) CheckGrant(context.Context, *principal.Principal, rbac.Role, rbac.Scope, []string, []string) error {
	return nil
}

// TestDirectoryOverPostgres runs a disable through the directory to check the token version bump lands in the column.
func TestDirectoryOverPostgres(t *testing.T) {
	ctx := context.Background()
	stores := New(openPool(t, migratedDB(t)))
	d, err := directory.New(directory.Config{Stores: stores, Grants: allowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	u := &directory.User{Username: "ada"}
	if err := d.CreateUser(ctx, nil, u); err != nil {
		t.Fatal(err)
	}
	if err := d.DisableUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, err := stores.Users.Get(ctx, u.ID)
	if err != nil || !got.Disabled || got.TokenVersion != 2 {
		t.Fatalf("after disable: %+v, %v", got, err)
	}
	team := &directory.Team{Meta: directory.Metadata{DisplayName: "Core Team"}}
	if err := d.CreateTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	dup := &directory.Team{Meta: directory.Metadata{DisplayName: "Core Team"}}
	if err := d.CreateTeam(ctx, dup); err != nil || dup.Meta.Name != "core-team-2" {
		t.Fatalf("second team = %q, %v", dup.Meta.Name, err)
	}
	clash := &directory.Team{Meta: directory.Metadata{ID: directory.NewID(), Name: "core-team", Owner: authz.Owner{Kind: "system"}}}
	if err := stores.Teams.Upsert(ctx, clash); !errors.Is(err, directory.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
}

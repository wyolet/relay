//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/meta"
)

// Relay's down-migrations drop and its up-migrations recreate tables the auth module's later migrations extend, so a rollback below them steps the module back too and the next boot brings both schemas to head again.
func TestRollbackBelowSharedTablesRestoresAuthSchema(t *testing.T) {
	t.Parallel()
	for _, target := range []uint{25, 0} {
		dsn := storagetest.EmptyDB(t)
		boot := func() {
			st, err := storagemod.Open(context.Background(), dsn)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			st.Close()
		}
		boot()
		if v := authVersion(t, dsn); v != authHead {
			t.Fatalf("auth schema at %d after boot, want %d", v, authHead)
		}
		if err := storagemod.MigrateTo(dsn, target); err != nil {
			t.Fatalf("migrate down to %d: %v", target, err)
		}
		if v := authVersion(t, dsn); v != 1 {
			t.Fatalf("auth schema at %d after relay went down to %d, want 1", v, target)
		}
		boot()
		if v := authVersion(t, dsn); v != authHead {
			t.Fatalf("auth schema at %d after coming back up from %d, want %d", v, target, authHead)
		}
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authpg.New(pool).Groups.List(context.Background()); err != nil {
			t.Errorf("groups after a round trip through %d: %v", target, err)
		}
		if _, err := authpg.New(pool).Users.List(context.Background()); err != nil {
			t.Errorf("users after a round trip through %d: %v", target, err)
		}
		pool.Close()
	}
}

// A rollback below the shared tables that a relay down migration refuses (000047's, while a team owns a service account) leaves both schemas as they were, so the next boot finds nothing to do and the rows are intact.
func TestRollbackRefusedByARelayDownLeavesBothSchemas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Global}}
	if err := team.NewStore(pool).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}}
	if err := serviceaccount.NewStore(pool).Upsert(ctx, bot); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}
	relayHead := storagetest.LatestVersion(t)

	err = storagemod.MigrateTo(dsn, 25)
	if err == nil || !strings.Contains(err.Error(), "owned by a team") {
		t.Fatalf("migrate down to 25 = %v, want 000047's refusal", err)
	}
	if v := authVersion(t, dsn); v != authHead {
		t.Fatalf("auth schema at %d after the refused rollback, want %d", v, authHead)
	}
	if v, dirty := relayVersion(t, dsn); v != int64(relayHead) || dirty {
		t.Fatalf("relay schema at %d (dirty %v) after the refused rollback, want %d clean", v, dirty, relayHead)
	}

	st, err = storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("boot after the refused rollback: %v", err)
	}
	st.Close()
	pool.Reset()
	if got, err := serviceaccount.NewStore(pool).Get(ctx, bot.Meta.ID); err != nil || got == nil {
		t.Fatalf("service account after the refused rollback = %v, %v", got, err)
	}
	if _, err := authpg.New(pool).Users.List(ctx); err != nil {
		t.Fatalf("users after the refused rollback: %v", err)
	}
}

func relayVersion(t *testing.T, dsn string) (int64, bool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var v int64
	var dirty bool
	if err := pool.QueryRow(context.Background(), "SELECT version, dirty FROM schema_migrations").Scan(&v, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return v, dirty
}

// authHead is the auth module's newest migration.
const authHead = 6

func authVersion(t *testing.T, dsn string) int64 {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var v int64
	if err := pool.QueryRow(context.Background(), "SELECT version FROM "+authpg.MigrationsTable).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", authpg.MigrationsTable, err)
	}
	return v
}

//go:build integration

// rotate_integration_test.go covers the conditional rotate against a real
// Postgres: the store's UPDATE ... WHERE key_hash = $old has no fake seam.
// Run with: make test-integration.
package key_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

func setupKeyDB(t *testing.T) (*key.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return key.NewStore(gen.New(pool)), ctx
}

// seedUser writes the users row the relay_keys principal FK needs.
func seedUser(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	u := &user.User{ID: meta.NewID(), Username: "rotate-" + meta.NewID()[:8]}
	store := user.NewStore(gen.New(pool))
	if err := store.Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, u.ID) })
	return u.ID
}

func hash64(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// Two rotations that both read the same stored hash: the second must be
// refused rather than overwrite the first, which would leave the plaintext
// handed to the first caller authenticating nothing.
func TestRotateIsConditionalOnTheReadHash(t *testing.T) {
	store, ctx := setupKeyDB(t)

	userID := seedUser(t, ctx)
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "rotate-race", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: userID}
	k.Spec.KeyHash = hash64('a')
	k.Spec.Prefix = "sk-wr-aaaa"
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	read, err := store.Get(ctx, k.Meta.ID)
	if err != nil || read == nil {
		t.Fatalf("get: %v", err)
	}
	original := read.Spec.KeyHash

	first := *read
	first.Spec.KeyHash = hash64('b')
	if err := store.Rotate(ctx, &first, original); err != nil {
		t.Fatalf("first rotate: %v", err)
	}

	second := *read
	second.Spec.KeyHash = hash64('c')
	if err := store.Rotate(ctx, &second, original); !errors.Is(err, key.ErrRotationRaced) {
		t.Fatalf("second rotate err = %v, want ErrRotationRaced", err)
	}

	after, err := store.Get(ctx, k.Meta.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.Spec.KeyHash != hash64('b') {
		t.Fatalf("stored hash = %q, want the first rotation's", after.Spec.KeyHash)
	}
	if err := store.Delete(ctx, k.Meta.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestRotateSucceedsOnTheCurrentHash(t *testing.T) {
	store, ctx := setupKeyDB(t)

	userID := seedUser(t, ctx)
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "rotate-ok", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: userID}
	k.Spec.KeyHash = hash64('d')
	k.Spec.Prefix = "sk-wr-dddd"
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	next := *k
	next.Spec.KeyHash = hash64('e')
	next.Spec.PreviousKeyHash = hash64('d')
	if err := store.Rotate(ctx, &next, hash64('d')); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after, err := store.Get(ctx, k.Meta.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.Spec.KeyHash != hash64('e') || after.Spec.PreviousKeyHash != hash64('d') {
		t.Fatalf("hashes = %q/%q after rotate", after.Spec.KeyHash, after.Spec.PreviousKeyHash)
	}
	if err := store.Delete(ctx, k.Meta.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

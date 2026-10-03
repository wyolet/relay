//go:build integration

// missing_ids_integration_test.go covers the batched membership check
// against a real Postgres — the query has no fake seam. Run with:
// make test-integration.
package user_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

func setupUserDB(t *testing.T) (*user.Store, context.Context) {
	t.Helper()
	return user.NewStore(gen.New(storagetest.Pool(t))), context.Background()
}

func TestMissingIDsReportsOnlyAbsentUsers(t *testing.T) {
	store, ctx := setupUserDB(t)

	present := &user.User{ID: meta.NewID(), Username: "member-" + meta.NewID()[:8]}
	if err := store.Upsert(ctx, present); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, present.ID) })

	absent := meta.NewID()
	missing, err := store.MissingIDs(ctx, []string{present.ID, absent})
	if err != nil {
		t.Fatalf("MissingIDs: %v", err)
	}
	if len(missing) != 1 || missing[0] != absent {
		t.Fatalf("missing = %v, want [%s]", missing, absent)
	}

	if missing, err := store.MissingIDs(ctx, nil); err != nil || len(missing) != 0 {
		t.Fatalf("MissingIDs(nil) = (%v, %v), want (nil, nil)", missing, err)
	}
	if missing, err := store.MissingIDs(ctx, []string{present.ID}); err != nil || len(missing) != 0 {
		t.Fatalf("MissingIDs on an existing user = (%v, %v), want none missing", missing, err)
	}
}

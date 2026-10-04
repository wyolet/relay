//go:build integration

// rotate_integration_test.go covers the conditional rotate against a real
// Postgres: the store's UPDATE ... WHERE key_hash = $old has no fake seam.
// Run with: make test-integration.
package key_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// setupKeyDB returns a key store on a fresh database and the id of the user
// row the relay_keys principal FK needs.
func setupKeyDB(t *testing.T) (*key.Store, context.Context, string) {
	t.Helper()
	ctx := context.Background()
	q := gen.New(storagetest.Pool(t))
	u := &user.User{ID: meta.NewID(), Username: "rotate-" + meta.NewID()[:8]}
	if err := user.NewStore(q).Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return key.NewStore(q), ctx, u.ID
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
	store, ctx, userID := setupKeyDB(t)

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
	if err := store.Rotate(ctx, &first, original, read.Meta.UpdatedAt); err != nil {
		t.Fatalf("first rotate: %v", err)
	}

	second := *read
	second.Spec.KeyHash = hash64('c')
	if err := store.Rotate(ctx, &second, original, read.Meta.UpdatedAt); !errors.Is(err, key.ErrRotationRaced) {
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
	store, ctx, userID := setupKeyDB(t)

	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "rotate-ok", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: userID}
	k.Spec.KeyHash = hash64('d')
	k.Spec.Prefix = "sk-wr-dddd"
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	read, err := store.Get(ctx, k.Meta.ID)
	if err != nil || read == nil {
		t.Fatalf("get: %v", err)
	}
	next := *read
	next.Spec.KeyHash = hash64('e')
	next.Spec.PreviousKeyHash = hash64('d')
	if err := store.Rotate(ctx, &next, hash64('d'), read.Meta.UpdatedAt); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after, err2 := store.Get(ctx, k.Meta.ID)
	if err2 != nil {
		t.Fatalf("re-read: %v", err2)
	}
	if after.Spec.KeyHash != hash64('e') || after.Spec.PreviousKeyHash != hash64('d') {
		t.Fatalf("hashes = %q/%q after rotate", after.Spec.KeyHash, after.Spec.PreviousKeyHash)
	}
	if err := store.Delete(ctx, k.Meta.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// A rotation is computed against the row the caller read; a plain update
// landing in between changes fields the rotation would write back stale.
// TestRotateLosesToAConcurrentUpdate pins that the rotation loses instead.
func TestRotateLosesToAConcurrentUpdate(t *testing.T) {
	store, ctx, userID := setupKeyDB(t)

	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "rotate-vs-update", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: userID}
	k.Spec.KeyHash = hash64('f')
	k.Spec.Prefix = "sk-wr-ffff"
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	read, err := store.Get(ctx, k.Meta.ID)
	if err != nil || read == nil {
		t.Fatalf("get: %v", err)
	}

	// Someone else edits the row through plain CRUD: same hash, new version.
	edited := *read
	edited.Meta.DisplayName = "renamed"
	if err := store.Upsert(ctx, &edited); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}

	rotated := *read
	rotated.Spec.KeyHash = hash64('g')
	if err := store.Rotate(ctx, &rotated, read.Spec.KeyHash, read.Meta.UpdatedAt); !errors.Is(err, key.ErrRotationRaced) {
		t.Fatalf("rotate err = %v, want ErrRotationRaced", err)
	}
	after, err := store.Get(ctx, k.Meta.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.Spec.KeyHash != hash64('f') || after.Meta.DisplayName != "renamed" {
		t.Fatalf("row = %q/%q, want the concurrent update to stand", after.Spec.KeyHash, after.Meta.DisplayName)
	}
	if err := store.Delete(ctx, k.Meta.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

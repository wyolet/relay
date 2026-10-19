//go:build integration

// keystore_integration_test.go covers the Postgres keyring.Store: its conditional writes and the destroy checks it makes inside one transaction against the database clock. Run with: make test-integration.

package stored_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret/keyring"
)

func TestIntegration_KeyStoreDestroyUsesTheDatabaseClock(t *testing.T) {
	pool := storagetest.Pool(t)
	b := backend(t, pool, keyA)
	if _, err := b.CreateScoped(ctx, "s", "v", []byte("v")); err != nil {
		t.Fatal(err)
	}
	old := keyID(t, rowOf(t, pool, "v").dek)
	if _, err := ringOf(t, b).Rotate(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RewriteStale(ctx); err != nil {
		t.Fatal(err)
	}
	ks := stored.NewKeyStore(pool)
	if err := ks.Destroy(ctx, old, time.Hour); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy just demoted: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE secret_data_keys SET demoted_at = NOW() - interval '2 hours' WHERE id = $1", old); err != nil {
		t.Fatal(err)
	}
	if err := ks.Destroy(ctx, old, time.Hour); err != nil {
		t.Fatalf("destroy after the wait: %v", err)
	}
	if err := ks.Destroy(ctx, old, time.Hour); err != nil {
		t.Fatalf("destroy again: %v", err)
	}
	k, err := ks.Get(ctx, old)
	if err != nil || k.State != keyring.Destroyed || k.Wrapped != nil || k.Scope != "s" {
		t.Fatalf("tombstone: %+v %v", k, err)
	}
	p, err := ks.Primary(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Destroy(ctx, p.ID, 0); !errors.Is(err, keyring.ErrIsPrimary) {
		t.Fatalf("destroy the Primary: %v", err)
	}
	if err := ks.Destroy(ctx, [16]byte{1}, 0); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("destroy an unknown key: %v", err)
	}
}

func TestIntegration_KeyStoreConditionalWrites(t *testing.T) {
	pool := storagetest.Pool(t)
	ks := stored.NewKeyStore(pool)
	kekA, kekB := [16]byte{0xa}, [16]byte{0xb}
	if err := ks.SetTargetKEK(ctx, kekB, kekA); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("set target from a wrong expected: %v", err)
	}
	if err := ks.SetTargetKEK(ctx, kekA, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.TargetKEK(ctx); err != nil || got != kekA {
		t.Fatalf("target = %x, %v", got, err)
	}
	if err := ks.SetTargetKEK(ctx, kekB, [16]byte{}); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("set target as if unset: %v", err)
	}

	k1 := keyring.DataKey{ID: [16]byte{1}, Scope: "s", State: keyring.Primary, KEKID: kekA, Wrapped: []byte("w1")}
	k2 := keyring.DataKey{ID: [16]byte{2}, Scope: "s", State: keyring.Primary, KEKID: kekA, Wrapped: []byte("w2")}
	if err := ks.InsertPrimary(ctx, k1, &k2.ID); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("insert expecting a Primary that is not there: %v", err)
	}
	if err := ks.InsertPrimary(ctx, k1, nil); err != nil {
		t.Fatal(err)
	}
	if err := ks.InsertPrimary(ctx, k2, nil); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("second first key of a scope: %v", err)
	}
	if err := ks.InsertPrimary(ctx, k2, &k1.ID); err != nil {
		t.Fatal(err)
	}
	demoted, err := ks.Get(ctx, k1.ID)
	if err != nil || demoted.State != keyring.DecryptOnly || demoted.DemotedAt.IsZero() {
		t.Fatalf("demoted key: %+v %v", demoted, err)
	}
	if err := ks.UpdateWrapped(ctx, k1.ID, kekB, kekA, []byte("x")); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("rewrap from a KEK the key is not under: %v", err)
	}
	if err := ks.UpdateWrapped(ctx, k1.ID, kekA, kekB, []byte("rewrapped")); err != nil {
		t.Fatal(err)
	}
	if k, _ := ks.Get(ctx, k1.ID); k.KEKID != kekB || string(k.Wrapped) != "rewrapped" {
		t.Fatalf("rewrapped key: %+v", k)
	}
	if err := ks.UpdateWrapped(ctx, [16]byte{9}, kekA, kekB, []byte("x")); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("rewrap an unknown key: %v", err)
	}
	all, err := ks.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %d keys, %v", len(all), err)
	}
}

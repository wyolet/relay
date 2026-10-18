package keyring_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

func TestRotateOldDecryptsStale(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	r := newRing(t, ms, c, newKEK(t, 1))
	old, oldID := mustEncrypt(t, r, "", "v1", "row")
	c.Advance(time.Minute)
	nk, err := r.Rotate(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if nk.ID == oldID || nk.State != keyring.Primary || nk.Wrapped == nil {
		t.Fatalf("Rotate = %+v", nk)
	}
	if st := mustDecrypt(t, r, "", old, "row", "v1"); !st.Stale || st.Reason != keyring.StaleNotPrimary {
		t.Fatalf("old value status = %+v", st)
	}
	fresh, id := mustEncrypt(t, r, "", "v2", "row")
	if id != nk.ID {
		t.Fatal("Encrypt did not use the new Primary")
	}
	if st := mustDecrypt(t, r, "", fresh, "row", "v2"); st.Stale {
		t.Fatal("new value stale")
	}
	info, _ := keyInfo(r, oldID)
	if info.State != keyring.DecryptOnly || !info.DemotedAt.Equal(c.Now()) {
		t.Fatalf("old key = %+v", info)
	}
	// Rotating a scope with no key creates its first one.
	if k, err := r.Rotate(ctx, "fresh"); err != nil || k.State != keyring.Primary {
		t.Fatalf("Rotate(fresh) = %+v, %v", k, err)
	}
}

// racingStore runs race once, just before the next InsertPrimary reaches the store.
type racingStore struct {
	keyring.Store
	race func()
}

func (s *racingStore) InsertPrimary(ctx context.Context, k keyring.DataKey, expected *[16]byte) error {
	if f := s.race; f != nil {
		s.race = nil
		f()
	}
	return s.Store.InsertPrimary(ctx, k, expected)
}

func TestRotateConflict(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	other := newRing(t, ms, c, k1)
	mustEncrypt(t, other, "s", "v", "")
	rs := &racingStore{Store: ms}
	r := newRing(t, rs, c, k1)
	var winner keyring.DataKey
	rs.race = func() {
		var err error
		if winner, err = other.Rotate(ctx, "s"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Rotate(ctx, "s"); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("Rotate during a concurrent rotate: %v", err)
	}
	if _, id := mustEncrypt(t, r, "s", "w", ""); id != winner.ID {
		t.Fatalf("after the conflict Encrypt used %s, want the winner %s", keyring.FormatID(id), keyring.FormatID(winner.ID))
	}
	if ks := keysOfScope(t, ms, "s"); len(ks) != 2 {
		t.Fatalf("scope keys = %+v", ks)
	}
}

func TestDestroy(t *testing.T) {
	c := newClock()
	rs := newRows()
	r := newRing(t, rs.ms, c, newKEK(t, 1))
	if err := r.EncryptAndWrite(ctx, "s", []byte("v"), []byte("x"), rs.writer("x", "s")); err != nil {
		t.Fatal(err)
	}
	oldID := rs.get("x").keyID
	oldSealed := rs.get("x").sealed
	if err := r.Destroy(ctx, oldID); !errors.Is(err, keyring.ErrIsPrimary) {
		t.Fatalf("destroy Primary: %v", err)
	}
	if err := r.ForceDestroy(ctx, oldID); !errors.Is(err, keyring.ErrIsPrimary) {
		t.Fatalf("force destroy Primary: %v", err)
	}
	if _, err := r.Rotate(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := r.Destroy(ctx, oldID); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy just demoted: %v", err)
	}
	c.Advance(24*time.Hour - time.Second)
	if err := r.Destroy(ctx, oldID); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy 1s early: %v", err)
	}
	c.Advance(time.Second)
	if err := r.Destroy(ctx, oldID); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("destroy in use: %v", err)
	}
	if err := r.ForceDestroy(ctx, oldID); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("force destroy in use: %v", err)
	}
	// Rewrite the row under the new Primary, then destroy.
	if err := r.EncryptAndWrite(ctx, "s", []byte("v"), []byte("x"), rs.writer("x", "s")); err != nil {
		t.Fatal(err)
	}
	other := newRing(t, rs.ms, c, newKEK(t, 1)) // holds the old DEK
	if err := r.Destroy(ctx, oldID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := r.Destroy(ctx, oldID); err != nil {
		t.Fatalf("destroy again: %v", err)
	}
	if _, _, err := r.Decrypt(ctx, "s", oldSealed, []byte("x")); !errors.Is(err, keyring.ErrDestroyed) {
		t.Fatalf("decrypt under destroyed key: %v", err)
	}
	if err := other.Reload(ctx, oldID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Decrypt(ctx, "s", oldSealed, []byte("x")); !errors.Is(err, keyring.ErrDestroyed) {
		t.Fatalf("other ring after reload: %v", err)
	}
	late := newRing(t, rs.ms, c, newKEK(t, 1))
	if _, _, err := late.Decrypt(ctx, "s", oldSealed, []byte("x")); !errors.Is(err, keyring.ErrDestroyed) {
		t.Fatalf("new ring: %v, want ErrDestroyed (tombstone), not unknown", err)
	}
	k, err := rs.ms.Get(ctx, oldID)
	if err != nil || k.State != keyring.Destroyed || k.Wrapped != nil || k.Scope != "s" {
		t.Fatalf("tombstone = %+v, %v", k, err)
	}
	if info, _ := keyInfo(late, oldID); info.State != keyring.Destroyed || info.Locked {
		t.Fatalf("Keys tombstone = %+v", info)
	}

	// Force skips only the age check.
	prev := rs.get("x").keyID
	if _, err := r.Rotate(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := r.EncryptAndWrite(ctx, "s", []byte("v"), []byte("x"), rs.writer("x", "s")); err != nil {
		t.Fatal(err)
	}
	if err := r.Destroy(ctx, prev); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy fresh: %v", err)
	}
	if err := r.ForceDestroy(ctx, prev); err != nil {
		t.Fatalf("force destroy: %v", err)
	}
	if err := r.Destroy(ctx, [16]byte{1}); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("destroy unknown: %v", err)
	}
}

func TestRewrap(t *testing.T) {
	c := newClock()
	rs := newRows()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	both := newRing(t, rs.ms, c, k1, k2)
	values := map[string]string{}
	for i, scope := range []string{"", "a", "b", "a"} {
		name := fmt.Sprint("r", i)
		values[name] = "v" + name
		if err := both.EncryptAndWrite(ctx, scope, []byte(values[name]), []byte(name), rs.writer(name, scope)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := both.Rotate(ctx, "a"); err != nil { // leaves a DecryptOnly key in use
		t.Fatal(err)
	}
	oldOnly := newRing(t, rs.ms, c, k1)

	if _, err := both.Rewrap(ctx, newKEK(t, 3).ID()); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("rewrap to a KEK not held: %v", err)
	}
	n, err := both.Rewrap(ctx, k2.ID())
	if err != nil || n != 4 {
		t.Fatalf("Rewrap = %d, %v; want 4 keys", n, err)
	}
	if got, _ := rs.ms.TargetKEK(ctx); got != k2.ID() || both.Target() != k2.ID() {
		t.Fatalf("target = %x", got)
	}
	all, _ := rs.ms.List(ctx)
	for _, k := range all {
		if k.KEKID != k2.ID() {
			t.Fatalf("key %s still under %x", keyring.FormatID(k.ID), k.KEKID)
		}
	}
	// A ring holding only the new KEK reads everything; one holding only the old KEK keeps the DEKs it already unwrapped.
	newOnly := newRing(t, rs.ms, c, k2)
	if err := oldOnly.ReloadAll(ctx); err != nil {
		t.Fatal(err)
	}
	for name, rw := range rs.snapshot() {
		mustDecrypt(t, newOnly, rw.scope, rw.sealed, name, values[name])
		mustDecrypt(t, oldOnly, rw.scope, rw.sealed, name, values[name])
	}
	for _, k := range oldOnly.Keys() {
		if k.Locked || k.KEKID != k2.ID() {
			t.Fatalf("old-KEK ring after reload: %+v", k)
		}
	}
	// New keys wrap under the target.
	mustEncrypt(t, both, "c", "v", "")
	if ks := keysOfScope(t, rs.ms, "c"); ks[0].KEKID != k2.ID() {
		t.Fatal("new key not under the target")
	}
	if n, err := both.Rewrap(ctx, k2.ID()); err != nil || n != 0 {
		t.Fatalf("second Rewrap = %d, %v", n, err)
	}
}

func TestRewrapSkipsLocked(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2, k3 := newKEK(t, 1), newKEK(t, 2), newKEK(t, 3)
	mustEncrypt(t, newRing(t, ms, c, k1), "a", "v", "")
	partial := newRing(t, ms, c, k2, k3)
	n, err := partial.Rewrap(ctx, k2.ID())
	var rwErr *keyring.RewrapError
	if n != 0 || !errors.As(err, &rwErr) || !errors.Is(err, keyring.ErrKEKUnavailable) || len(rwErr.Skipped) != 1 {
		t.Fatalf("Rewrap = %d, %v", n, err)
	}
	if got, _ := ms.TargetKEK(ctx); got != k2.ID() {
		t.Fatal("target not moved")
	}
	if n, err := newRing(t, ms, c, k1, k2).Rewrap(ctx, k2.ID()); err != nil || n != 1 {
		t.Fatalf("rerun with both KEKs = %d, %v", n, err)
	}
}

func TestKeysCarryNoMaterial(t *testing.T) {
	typ := reflect.TypeFor[keyring.KeyInfo]()
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() == reflect.Slice {
			t.Fatalf("KeyInfo.%s could carry key material", typ.Field(i).Name)
		}
	}
}

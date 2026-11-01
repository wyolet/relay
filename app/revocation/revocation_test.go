package revocation

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

func TestRevokeReachesEveryPodOnRefresh(t *testing.T) {
	ctx := context.Background()
	store := kv.NewMem()
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	a, b := New(store), New(store)

	if err := a.Revoke(ctx, "jti-1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !a.Set().Revoked("jti-1", now) {
		t.Fatal("the revoking pod must deny the token at once")
	}
	if b.Set().Revoked("jti-1", now) {
		t.Fatal("another pod learns of it only on refresh")
	}
	if err := b.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.Set().Revoked("jti-1", now) || b.Set().Revoked("jti-2", now) {
		t.Fatal("refresh must copy exactly the shared list")
	}
	if b.Set().Revoked("jti-1", now.Add(2*time.Hour)) {
		t.Fatal("an entry past the token's expiry no longer denies")
	}
}

func TestExpiredEntriesArePruned(t *testing.T) {
	ctx := context.Background()
	store := kv.NewMem()
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	l := New(store)
	l.now = func() time.Time { return now }
	if err := l.Revoke(ctx, "old", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := l.Revoke(ctx, "new", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m, err := l.read(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["old"]; ok || len(m) != 1 {
		t.Fatalf("list = %v, want only the live entry", m)
	}
	if err := l.Revoke(ctx, "gone", now.Add(-time.Second)); err != nil || l.Set().Revoked("gone", now) {
		t.Fatalf("an already expired token is not recorded: %v", err)
	}
}

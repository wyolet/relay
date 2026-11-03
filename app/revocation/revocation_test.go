package revocation

import (
	"context"
	"sync/atomic"
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

// pausedRead holds the first Get after it has read, until released: a refresh caught between its read and its replace.
type pausedRead struct {
	Store
	paused   atomic.Bool
	read     chan struct{}
	released chan struct{}
}

func (p *pausedRead) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := p.Store.Get(ctx, key)
	if p.paused.CompareAndSwap(false, true) {
		close(p.read)
		<-p.released
	}
	return v, err
}

// A refresh that read the list before this pod revoked a token must not erase the revocation when it lands after it; the next read that holds the id takes over.
func TestRefreshReadBeforeARevokeKeepsIt(t *testing.T) {
	ctx := context.Background()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	store := &pausedRead{Store: mem, read: make(chan struct{}), released: make(chan struct{})}
	l := New(store)
	now := time.Now()

	refreshed := make(chan error, 1)
	go func() { refreshed <- l.Refresh(ctx) }()
	<-store.read
	if err := l.Revoke(ctx, "jti-1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	close(store.released)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if !l.Set().Revoked("jti-1", now) {
		t.Fatal("a refresh that read before the revocation erased it")
	}
	if err := l.Refresh(ctx); err != nil || !l.Set().Revoked("jti-1", now) || len(l.pending) != 0 {
		t.Fatalf("after a read holding the id: revoked=%v pending=%v err=%v; want it revoked from the list alone", l.Set().Revoked("jti-1", now), l.pending, err)
	}
}

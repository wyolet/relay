package revocation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	exp := time.Unix(now.Add(time.Hour).Unix(), 0)
	if !b.Set().Revoked("jti-1", exp.Add(-time.Second)) || b.Set().Revoked("jti-1", exp) {
		t.Fatal("an entry denies until the token's expiry and not from it on")
	}
	var none *Set
	if none.Revoked("jti-1", now) || b.Set().Revoked("", now) || new(Set).Revoked("jti-1", now) {
		t.Fatal("a missing set, an unfilled one, or an empty token id denies")
	}
}

// flakyStore is a single-key Store that fails every call while down and records the TTL of its last write. The List's Run goroutine reads it concurrently with the test, hence the mutex.
type flakyStore struct {
	mu   sync.Mutex
	raw  []byte
	ttl  time.Duration
	down bool
}

func (s *flakyStore) update(f func(*flakyStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *flakyStore) lastTTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

func (s *flakyStore) Get(context.Context, string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errors.New("store down")
	}
	if s.raw == nil {
		return nil, kv.ErrNotFound
	}
	return s.raw, nil
}

func (s *flakyStore) Set(_ context.Context, _ string, v []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return errors.New("store down")
	}
	s.raw, s.ttl = v, ttl
	return nil
}

func (s *flakyStore) WithLock(ctx context.Context, _ []string, fn func(context.Context) error) error {
	return fn(ctx)
}

func pendingLen(l *List) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}

// A revocation the shared list did not take is not served from this pod alone, a failed poll keeps the previous copy, and the stored key lives as long as its latest entry.
func TestStoreFailuresKeepThePreviousCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		store := &flakyStore{}
		l, other := New(store), New(store)
		go l.Run(ctx, time.Minute)
		revoked := func(id string) bool { return l.Set().Revoked(id, time.Now()) }

		if err := other.Revoke(ctx, "short", time.Now().Add(10*time.Minute)); err != nil || store.lastTTL() != 10*time.Minute {
			t.Fatalf("Revoke: err %v, key TTL %v; want 10m", err, store.lastTTL())
		}
		if err := other.Revoke(ctx, "long", time.Now().Add(time.Hour)); err != nil || store.lastTTL() != time.Hour {
			t.Fatalf("Revoke: err %v, key TTL %v; want the latest expiry, 1h", err, store.lastTTL())
		}
		synctest.Sleep(time.Minute)
		if !revoked("short") || !revoked("long") {
			t.Fatal("the poll did not pick up another pod's revocations")
		}

		store.update(func(s *flakyStore) { s.down = true })
		if err := l.Revoke(ctx, "unsaved", time.Now().Add(time.Hour)); err == nil || revoked("unsaved") || pendingLen(l) != 0 {
			t.Fatalf("Revoke with the store down: err %v, served locally %v", err, revoked("unsaved"))
		}
		if err := l.Refresh(ctx); err == nil {
			t.Fatal("Refresh with the store down reported success")
		}
		synctest.Sleep(time.Minute)
		if !revoked("long") {
			t.Fatal("a failed poll dropped the previous copy")
		}

		store.update(func(s *flakyStore) { s.down, s.raw = false, []byte(`{"fresh":99999999999}`) })
		synctest.Sleep(time.Minute)
		if !revoked("fresh") || revoked("long") {
			t.Fatal("the poll after the store came back did not replace the copy")
		}
		store.update(func(s *flakyStore) { s.raw = []byte(`not json`) })
		if err := l.Refresh(ctx); err == nil || !revoked("fresh") {
			t.Fatalf("Refresh over a corrupt list: err %v, previous copy kept %v", err, revoked("fresh"))
		}
		cancel()
	})
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

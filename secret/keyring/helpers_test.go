package keyring_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/secret/kek"
	"github.com/wyolet/relay/secret/kek/local"
	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

var ctx = context.Background()

func newKEK(t testing.TB, b byte) *local.KEK {
	t.Helper()
	k, err := local.New(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newSet(t testing.TB, keks ...kek.KEK) *kek.Set {
	t.Helper()
	s, err := kek.NewSet(keks...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// rows is a product's row store over memstore: writes go through RequirePrimary, and Destroy asks it whether a key is in use.
type rows struct {
	ms      *memstore.Store
	mu      sync.Mutex
	byName  map[string]row
	refused atomic.Int64
}

type row struct {
	scope  string
	sealed []byte
	keyID  [16]byte
}

func newRows(opts ...memstore.Option) *rows {
	r := &rows{byName: map[string]row{}}
	r.ms = memstore.New(append([]memstore.Option{memstore.WithInUse(r.inUse)}, opts...)...)
	return r
}

func (r *rows) inUse(id [16]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rw := range r.byName {
		if rw.keyID == id {
			return true
		}
	}
	return false
}

func (r *rows) writer(name, scope string) func([]byte, [16]byte) error {
	return func(sealed []byte, keyID [16]byte) error {
		err := r.ms.RequirePrimary(keyID, scope, func() error {
			r.mu.Lock()
			r.byName[name] = row{scope: scope, sealed: sealed, keyID: keyID}
			r.mu.Unlock()
			return nil
		})
		if errors.Is(err, keyring.ErrNotPrimary) {
			r.refused.Add(1)
		}
		return err
	}
}

func (r *rows) get(name string) row {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byName[name]
}

func (r *rows) snapshot() map[string]row {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]row, len(r.byName))
	for k, v := range r.byName {
		out[k] = v
	}
	return out
}

// counting wraps a Store and counts reads.
type counting struct {
	keyring.Store
	gets, primaries, lists atomic.Int64
	hidePrimary            atomic.Int64 // Primary answers ErrUnknownKey this many times
	listErr                atomic.Pointer[error]
}

func (c *counting) Get(ctx context.Context, id [16]byte) (keyring.DataKey, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, id)
}

func (c *counting) Primary(ctx context.Context, scope string) (keyring.DataKey, error) {
	c.primaries.Add(1)
	if c.hidePrimary.Add(-1) >= 0 {
		return keyring.DataKey{}, keyring.ErrUnknownKey
	}
	return c.Store.Primary(ctx, scope)
}

func (c *counting) List(ctx context.Context) ([]keyring.DataKey, error) {
	c.lists.Add(1)
	if p := c.listErr.Load(); p != nil {
		return nil, *p
	}
	return c.Store.List(ctx)
}

// lying wraps a Store and alters what InsertPrimary saves or what Get returns, as a faulty store would.
type lying struct {
	keyring.Store
	onInsert func(*keyring.DataKey)
	onGet    func(*keyring.DataKey)
}

func (l *lying) InsertPrimary(ctx context.Context, k keyring.DataKey, expected *[16]byte) error {
	if l.onInsert != nil {
		k.Wrapped = bytes.Clone(k.Wrapped)
		l.onInsert(&k)
	}
	return l.Store.InsertPrimary(ctx, k, expected)
}

func (l *lying) Get(ctx context.Context, id [16]byte) (keyring.DataKey, error) {
	k, err := l.Store.Get(ctx, id)
	if err == nil && l.onGet != nil {
		l.onGet(&k)
	}
	return k, err
}

// gated wraps a Store; once armed, the next Get closes entered and blocks until release or until its ctx ends, then panics when panics is set.
type gated struct {
	keyring.Store
	armed            atomic.Bool
	panics           bool
	entered, release chan struct{}
}

func newGated(st keyring.Store, panics bool) *gated {
	return &gated{Store: st, panics: panics, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gated) Get(ctx context.Context, id [16]byte) (keyring.DataKey, error) {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return keyring.DataKey{}, ctx.Err()
		}
		if g.panics {
			panic("store failed")
		}
	}
	return g.Store.Get(ctx, id)
}

func newRing(t testing.TB, st keyring.Store, c *clock, keks ...kek.KEK) *keyring.Ring {
	t.Helper()
	r, err := keyring.New(ctx, st, newSet(t, keks...), keyring.Options{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustEncrypt(t testing.TB, r *keyring.Ring, scope, plain, aad string) ([]byte, [16]byte) {
	t.Helper()
	sealed, id, err := r.Encrypt(ctx, scope, []byte(plain), []byte(aad))
	if err != nil {
		t.Fatalf("Encrypt(%q): %v", scope, err)
	}
	return sealed, id
}

func mustDecrypt(t testing.TB, r *keyring.Ring, scope string, sealed []byte, aad, want string) keyring.Status {
	t.Helper()
	got, st, err := r.Decrypt(ctx, scope, sealed, []byte(aad))
	if err != nil {
		t.Fatalf("Decrypt(%q): %v", scope, err)
	}
	if string(got) != want {
		t.Fatalf("Decrypt(%q) = %q, want %q", scope, got, want)
	}
	return st
}

func keysOfScope(t testing.TB, st keyring.Store, scope string) []keyring.DataKey {
	t.Helper()
	all, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []keyring.DataKey
	for _, k := range all {
		if k.Scope == scope {
			out = append(out, k)
		}
	}
	return out
}

func keyInfo(r *keyring.Ring, id [16]byte) (keyring.KeyInfo, bool) {
	for _, k := range r.Keys() {
		if k.ID == id {
			return k, true
		}
	}
	return keyring.KeyInfo{}, false
}

package token

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// sharedStore stands in for storage every replica reads, counting loads.
type sharedStore struct {
	mu    sync.Mutex
	keys  Keys
	err   error
	gate  chan struct{} // when set, a load blocks until it is closed
	loads atomic.Int32
}

func (s *sharedStore) LoadKeys(ctx context.Context) (Keys, error) {
	s.loads.Add(1)
	s.mu.Lock()
	gate, keys, err := s.gate, s.keys, s.err
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Keys{}, ctx.Err()
		}
	}
	return keys, err
}

func (s *sharedStore) set(k Keys) {
	s.mu.Lock()
	s.keys = k
	s.mu.Unlock()
}

func signingKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv := testKey(t)
	return priv
}

func loaded(t *testing.T, store KeyStore, opts KeySetOptions) *KeySet {
	t.Helper()
	ks := NewKeySet(store, opts)
	if err := ks.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return ks
}

func pubOf(priv ed25519.PrivateKey) ed25519.PublicKey {
	pub, _ := priv.Public().(ed25519.PublicKey)
	return pub
}

func TestKeySet_SignAndVerifyAcrossARotation(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := signingKey(t), signingKey(t)
	store := NewStaticStore(Keys{Signing: oldKey})
	ks := loaded(t, store, KeySetOptions{})

	verify := func(tok string, wantSub string, wantErr error) {
		t.Helper()
		got, err := Verify[testClaims](ctx, ks, tok)
		if err != wantErr || got.Sub != wantSub {
			t.Errorf("Verify = (sub %q, %v), want (sub %q, %v)", got.Sub, err, wantSub, wantErr)
		}
	}

	old, err := ks.Sign(testClaims{Sub: "user:u1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if HeaderKeyID(old) != KeyID(pubOf(oldKey)) {
		t.Fatalf("kid = %q, want KeyID of the signing key", HeaderKeyID(old))
	}
	bare := mustSign(t, oldKey, "", testClaims{Sub: "user:bare"})
	verify(old, "user:u1", nil)
	verify(bare, "user:bare", nil)

	// The previous list repeats both keys: each must still be published once.
	store.Set(Keys{Signing: newKey, Previous: []ed25519.PublicKey{pubOf(oldKey), pubOf(newKey), pubOf(oldKey)}})
	if err := ks.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	fresh, err := ks.Sign(testClaims{Sub: "user:u2"})
	must(t, err)
	if HeaderKeyID(fresh) != KeyID(pubOf(newKey)) {
		t.Fatalf("kid after rotation = %q, want KeyID of the new signing key", HeaderKeyID(fresh))
	}
	verify(fresh, "user:u2", nil)
	verify(old, "user:u1", nil)
	// A kid-less token is checked against the current key only.
	verify(bare, "", ErrSignature)
	// The old key's signature under the new key's kid, and the reverse.
	verify(mustSign(t, oldKey, KeyID(pubOf(newKey)), testClaims{Sub: "user:forged"}), "", ErrSignature)
	verify(mustSign(t, newKey, KeyID(pubOf(oldKey)), testClaims{Sub: "user:forged"}), "", ErrSignature)

	if d := cmp.Diff([]ed25519.PublicKey{pubOf(newKey), pubOf(oldKey)}, ks.PublicKeys()); d != "" {
		t.Errorf("PublicKeys, want current then previous (-want +got):\n%s", d)
	}
	wantSet, err := JWKSet(pubOf(newKey), pubOf(oldKey))
	must(t, err)
	set, err := ks.JWKSet()
	if d := cmp.Diff(wantSet, set); d != "" || err != nil {
		t.Errorf("JWKSet (%v) (-want +got):\n%s", err, d)
	}
	if d := cmp.Diff(Keys{Signing: newKey, Current: pubOf(newKey), Previous: []ed25519.PublicKey{pubOf(oldKey), pubOf(newKey), pubOf(oldKey)}}, ks.Keys()); d != "" {
		t.Errorf("Keys (-want +got):\n%s", d)
	}

	// Rotated out: the old key no longer verifies anything.
	store.Set(Keys{Signing: newKey})
	if err := ks.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	ks.opts.MinReloadInterval = -1
	verify(old, "", ErrUnknownKey)
	verify(fresh, "user:u2", nil)

	// No keys: tokens are off.
	store.Set(Keys{})
	if err := ks.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	verify(fresh, "", ErrUnknownKey)
	verify(bare, "", ErrUnknownKey)
	if ks.Current() != nil || len(ks.PublicKeys()) != 0 {
		t.Errorf("an empty store left keys installed: %v", ks.PublicKeys())
	}
}

func TestKeySet_BeforeAnyLoad(t *testing.T) {
	ks := NewKeySet(&StaticStore{}, KeySetOptions{MinReloadInterval: -1})
	if ks.Current() != nil || ks.PublicKeys() != nil || !cmp.Equal(ks.Keys(), Keys{}) {
		t.Errorf("an unloaded key set holds keys: %+v", ks.Keys())
	}
	if _, err := ks.PublicKey(context.Background(), ""); err != ErrUnknownKey {
		t.Errorf("PublicKey = %v, want ErrUnknownKey", err)
	}
	if set, err := ks.JWKSet(); err != nil || set.Keys == nil || len(set.Keys) != 0 {
		t.Errorf("JWKSet = %+v, %v; want an empty, non-nil key list", set, err)
	}
	if _, err := ks.Sign(testClaims{}); err != ErrNoSigningKey {
		t.Errorf("Sign = %v, want ErrNoSigningKey", err)
	}
}

func TestKeySet_UnknownKidIsASignatureError(t *testing.T) {
	ks := loaded(t, NewStaticStore(Keys{Signing: signingKey(t)}), KeySetOptions{MinReloadInterval: -1})
	_, err := Verify[testClaims](context.Background(), ks, mustSign(t, signingKey(t), "deadbeefdeadbeef", testClaims{}))
	if !errors.Is(err, ErrUnknownKey) || !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrUnknownKey wrapping ErrSignature", err)
	}
}

func TestKeySet_VerifyOnlyHasNoSigningKey(t *testing.T) {
	ks := loaded(t, NewStaticStore(Keys{Current: pubOf(signingKey(t))}), KeySetOptions{})
	if _, err := ks.Sign(testClaims{}); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("Sign = %v, want ErrNoSigningKey", err)
	}
}

func TestKeySet_InvalidKeysKeepTheInstalledSet(t *testing.T) {
	priv := signingKey(t)
	store := NewStaticStore(Keys{Signing: priv})
	ks := loaded(t, store, KeySetOptions{})
	for want, bad := range map[string]Keys{
		"token: signing key is 10 bytes, want 64":                 {Signing: priv[:10]},
		"token: verification key is 1 bytes, want 32":             {Signing: priv, Previous: []ed25519.PublicKey{{1}}},
		"token: verification key is 3 bytes, want 32":             {Current: ed25519.PublicKey{1, 2, 3}},
		"token: current key is not the signing key's public half": {Signing: priv, Current: pubOf(signingKey(t))},
	} {
		name := want
		store.Set(bad)
		if err := ks.Reload(context.Background()); err == nil || err.Error() != want {
			t.Errorf("Reload = %v, want %q", err, want)
		}
		if !ks.Current().Equal(pubOf(priv)) {
			t.Fatalf("%s: the installed key was replaced", name)
		}
	}
}

func TestKeySet_StoreErrorKeepsTheKeys(t *testing.T) {
	priv := signingKey(t)
	store := &sharedStore{keys: Keys{Signing: priv}}
	ks := loaded(t, store, KeySetOptions{})
	store.mu.Lock()
	down := errors.New("store down")
	store.err = down
	store.mu.Unlock()
	if err := ks.Reload(context.Background()); !errors.Is(err, down) {
		t.Fatalf("Reload = %v, want it to wrap the store error", err)
	}
	if !ks.Current().Equal(pubOf(priv)) {
		t.Fatal("a failed load dropped the keys")
	}
}

// Replica B never saw the rotation replica A performed; the first token
// naming the new kid makes it read the shared store.
func TestKeySet_RotationReachesAnotherReplica(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := signingKey(t), signingKey(t)
	store := &sharedStore{keys: Keys{Signing: oldKey}}
	a := loaded(t, store, KeySetOptions{})
	b := loaded(t, store, KeySetOptions{MinReloadInterval: time.Nanosecond})
	old, err := a.Sign(testClaims{Sub: "user:u1"})
	must(t, err)

	store.set(Keys{Signing: newKey, Previous: []ed25519.PublicKey{pubOf(oldKey)}})
	must(t, a.Reload(ctx))
	fresh, err := a.Sign(testClaims{Sub: "user:u2"})
	must(t, err)
	if _, err := Verify[testClaims](ctx, b, fresh); err != nil {
		t.Fatalf("replica B on a token under the new key: %v", err)
	}
	if _, err := Verify[testClaims](ctx, b, old); err != nil {
		t.Fatalf("replica B on a token under the previous key: %v", err)
	}
	if !b.Current().Equal(pubOf(newKey)) {
		t.Fatal("replica B did not install the rotated key")
	}
}

func TestKeySet_KidSprayIsRateLimited(t *testing.T) {
	store := &sharedStore{keys: Keys{Signing: signingKey(t)}}
	ks := loaded(t, store, KeySetOptions{MinReloadInterval: time.Hour})
	before := store.loads.Load()
	for i := range 1000 {
		if _, err := ks.PublicKey(context.Background(), fmt.Sprintf("%016x", i)); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("kid %d: %v", i, err)
		}
	}
	if n := store.loads.Load() - before; n != 0 {
		t.Fatalf("1000 unknown kids within the interval caused %d loads, want 0", n)
	}

	fresh := loaded(t, store, KeySetOptions{MinReloadInterval: time.Hour})
	fresh.lastLoad = time.Time{}
	before = store.loads.Load()
	for i := range 1000 {
		_, _ = fresh.PublicKey(context.Background(), fmt.Sprintf("%016x", i))
	}
	if n := store.loads.Load() - before; n != 1 {
		t.Fatalf("1000 unknown kids caused %d loads, want exactly 1", n)
	}
}

func TestKeySet_ConcurrentMissesShareOneLoad(t *testing.T) {
	store := &sharedStore{keys: Keys{Signing: signingKey(t)}}
	ks := NewKeySet(store, KeySetOptions{})
	gate := make(chan struct{})
	store.gate = gate

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { _, _ = ks.PublicKey(context.Background(), "") })
	}
	// Every caller is either the loader or waiting on it until the gate opens.
	for store.loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := store.loads.Load(); n != 1 {
		t.Fatalf("50 concurrent misses caused %d loads, want 1", n)
	}
	if ks.Current() == nil {
		t.Fatal("the shared load installed nothing")
	}
}

// A waiter whose request ends stops waiting; the load it joined carries on.
func TestKeySet_WaiterHonoursItsContext(t *testing.T) {
	store := &sharedStore{keys: Keys{Signing: signingKey(t)}}
	ks := NewKeySet(store, KeySetOptions{LoadTimeout: time.Minute})
	gate := make(chan struct{})
	store.gate = gate
	go func() { _, _ = ks.PublicKey(context.Background(), "") }()
	for store.loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := ks.PublicKey(ctx, ""); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey once the waiter's context ends", err)
	}
	close(gate)
}

func TestKeySet_ReloadOffNeverReadsTheStore(t *testing.T) {
	store := &sharedStore{keys: Keys{Signing: signingKey(t)}}
	ks := loaded(t, store, KeySetOptions{MinReloadInterval: -1})
	ks.lastLoad = time.Time{}
	before := store.loads.Load()
	_, _ = ks.PublicKey(context.Background(), "0011223344556677")
	if n := store.loads.Load() - before; n != 0 {
		t.Fatalf("an unknown kid caused %d loads with reloads off", n)
	}
}

func TestKeySet_RunRefreshes(t *testing.T) {
	store := &sharedStore{}
	var errs atomic.Int32
	ks := NewKeySet(store, KeySetOptions{RefreshInterval: time.Millisecond, OnError: func(error) { errs.Add(1) }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ks.Run(ctx)

	priv := signingKey(t)
	store.set(Keys{Signing: priv})
	deadline := time.Now().Add(5 * time.Second)
	for !ks.Current().Equal(pubOf(priv)) {
		if time.Now().After(deadline) {
			t.Fatal("Run never installed the stored key")
		}
		time.Sleep(time.Millisecond)
	}

	store.mu.Lock()
	store.err = errors.New("store down")
	store.mu.Unlock()
	for errs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never reported a failed load")
		}
		time.Sleep(time.Millisecond)
	}
}

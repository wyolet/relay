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

	old, err := ks.Sign(testClaims{Sub: "user:u1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if HeaderKeyID(old) != KeyID(pubOf(oldKey)) {
		t.Fatalf("kid = %q, want KeyID of the signing key", HeaderKeyID(old))
	}
	bare, err := Sign(oldKey, "", testClaims{Sub: "user:bare"})
	if err != nil {
		t.Fatal(err)
	}

	store.Set(Keys{Signing: newKey, Previous: []ed25519.PublicKey{pubOf(oldKey)}})
	if err := ks.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	fresh, err := ks.Sign(testClaims{Sub: "user:u2"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"new key": fresh, "previous key": old} {
		if _, err := Verify[testClaims](ctx, ks, tok); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A kid-less token is checked against the current key only.
	if _, err := Verify[testClaims](ctx, ks, bare); !errors.Is(err, ErrSignature) {
		t.Errorf("bare token under the previous key: %v, want ErrSignature", err)
	}
	if got := ks.PublicKeys(); len(got) != 2 || !got[0].Equal(pubOf(newKey)) || !got[1].Equal(pubOf(oldKey)) {
		t.Errorf("PublicKeys = %v, want current then previous", got)
	}
	set, err := ks.JWKSet()
	if err != nil || len(set.Keys) != 2 || set.Keys[0].Kid != KeyID(pubOf(newKey)) {
		t.Errorf("JWKSet = %+v, %v; want both keys, current first", set, err)
	}
}

func TestKeySet_UnknownKidIsASignatureError(t *testing.T) {
	ks := loaded(t, NewStaticStore(Keys{Signing: signingKey(t)}), KeySetOptions{MinReloadInterval: -1})
	tok, err := Sign(signingKey(t), "deadbeefdeadbeef", testClaims{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify[testClaims](context.Background(), ks, tok)
	if !errors.Is(err, ErrUnknownKey) || !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrUnknownKey wrapping ErrSignature", err)
	}
}

func TestKeySet_VerifyOnlyHasNoSigningKey(t *testing.T) {
	ks := loaded(t, NewStaticStore(Keys{Current: pubOf(signingKey(t))}), KeySetOptions{})
	if _, err := ks.Sign(testClaims{}); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("Sign = %v, want ErrNoSigningKey", err)
	}
	if _, err := NewKeySet(&StaticStore{}, KeySetOptions{}).Sign(testClaims{}); !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("Sign before any load = %v, want ErrNoSigningKey", err)
	}
}

func TestKeySet_InvalidKeysKeepTheInstalledSet(t *testing.T) {
	priv := signingKey(t)
	store := NewStaticStore(Keys{Signing: priv})
	ks := loaded(t, store, KeySetOptions{})
	for name, bad := range map[string]Keys{
		"short signing key":     {Signing: priv[:10]},
		"short previous key":    {Signing: priv, Previous: []ed25519.PublicKey{{1}}},
		"current is not signer": {Signing: priv, Current: pubOf(signingKey(t))},
	} {
		store.Set(bad)
		if err := ks.Reload(context.Background()); err == nil {
			t.Errorf("%s: Reload accepted it", name)
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
	store.err = errors.New("store down")
	store.mu.Unlock()
	if err := ks.Reload(context.Background()); err == nil {
		t.Fatal("Reload hid the store error")
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
	if err != nil {
		t.Fatal(err)
	}

	store.set(Keys{Signing: newKey, Previous: []ed25519.PublicKey{pubOf(oldKey)}})
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := a.Sign(testClaims{Sub: "user:u2"})
	if err != nil {
		t.Fatal(err)
	}
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

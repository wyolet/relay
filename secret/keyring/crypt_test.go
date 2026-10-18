package keyring_test

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"sync"
	"testing"

	"github.com/wyolet/relay/secret/aead"
	"github.com/wyolet/relay/secret/kek"
	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

func TestFirstKeySetsTarget(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	r := newRing(t, ms, c, k1, k2)
	if r.Target() != ([16]byte{}) || len(r.Keys()) != 0 {
		t.Fatal("fresh ring has state")
	}
	sealed, id := mustEncrypt(t, r, "user:1", "sk-1", "row/1")
	if got, _ := ms.TargetKEK(ctx); got != k1.ID() {
		t.Fatalf("target = %x, want first KEK %x", got, k1.ID())
	}
	if r.Target() != k1.ID() {
		t.Fatal("ring target not updated")
	}
	if st := mustDecrypt(t, r, "user:1", sealed, "row/1", "sk-1"); st.Stale {
		t.Fatal("fresh value stale")
	}
	keys := r.Keys()
	if len(keys) != 1 || keys[0].ID != id || keys[0].State != keyring.Primary || keys[0].KEKID != k1.ID() || keys[0].Locked {
		t.Fatalf("Keys = %+v", keys)
	}
	// UUIDv7: clock milliseconds, version 7, RFC variant.
	ms48 := uint64(id[0])<<40 | uint64(id[1])<<32 | uint64(id[2])<<24 | uint64(id[3])<<16 | uint64(id[4])<<8 | uint64(id[5])
	if ms48 != uint64(c.Now().UnixMilli()) || id[6]>>4 != 7 || id[8]>>6 != 2 {
		t.Fatalf("key id %s is not a UUIDv7 at the clock", keyring.FormatID(id))
	}
	if !keys[0].CreatedAt.Equal(c.Now()) {
		t.Fatalf("CreatedAt = %v", keys[0].CreatedAt)
	}
	// Another scope reuses the target.
	_, id2 := mustEncrypt(t, r, "user:2", "sk-2", "row/2")
	if id2 == id {
		t.Fatal("scopes share a key")
	}
	if ks := keysOfScope(t, ms, "user:2"); len(ks) != 1 || ks[0].KEKID != k1.ID() {
		t.Fatalf("user:2 keys = %+v", ks)
	}
}

func TestRingWithoutTargetRefusesCreateButDecrypts(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	both := newRing(t, ms, c, k1, k2)
	sealed, _ := mustEncrypt(t, both, "a", "v", "row")
	oldOnly := newRing(t, ms, c, k1)
	if _, err := both.Rewrap(ctx, k2.ID()); err != nil {
		t.Fatal(err)
	}
	if err := oldOnly.ReloadAll(ctx); err != nil {
		t.Fatal(err)
	}
	mustDecrypt(t, oldOnly, "a", sealed, "row", "v")
	if _, _, err := oldOnly.Encrypt(ctx, "b", []byte("x"), nil); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("Encrypt new scope: err = %v, want ErrTargetKEKUnavailable", err)
	}
	if ks := keysOfScope(t, ms, "b"); len(ks) != 0 {
		t.Fatalf("scope b got keys: %+v", ks)
	}
	// An existing Primary it holds still encrypts.
	s2, _ := mustEncrypt(t, oldOnly, "a", "w", "row")
	mustDecrypt(t, both, "a", s2, "row", "w")

	// A ring holding only an unrelated key creates nothing and stays up.
	stranger := newRing(t, ms, c, newKEK(t, 9))
	if _, _, err := stranger.Encrypt(ctx, "c", []byte("x"), nil); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("stranger Encrypt: %v", err)
	}
	if _, _, err := stranger.Decrypt(ctx, "a", sealed, []byte("row")); !errors.Is(err, keyring.ErrKEKUnavailable) {
		t.Fatalf("stranger Decrypt: %v", err)
	}
	for _, k := range stranger.Keys() {
		if !k.Locked {
			t.Fatalf("stranger holds %+v", k)
		}
	}
}

func TestConcurrentFirstKey(t *testing.T) {
	for round := 0; round < 20; round++ {
		c := newClock()
		ms := memstore.New()
		k1 := newKEK(t, 1)
		rings := []*keyring.Ring{newRing(t, ms, c, k1), newRing(t, ms, c, k1)}
		type out struct {
			sealed []byte
			ring   int
		}
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			mu      sync.Mutex
			outputs []out
		)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				sealed, _, err := rings[i%2].Encrypt(ctx, "s", []byte("v"), nil)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				outputs = append(outputs, out{sealed, i % 2})
				mu.Unlock()
			}(i)
		}
		close(start)
		wg.Wait()
		ks := keysOfScope(t, ms, "s")
		if len(ks) != 1 || ks[0].State != keyring.Primary {
			t.Fatalf("round %d: scope keys = %+v", round, ks)
		}
		for _, o := range outputs {
			for _, r := range rings {
				mustDecrypt(t, r, "s", o.sealed, "", "v")
			}
		}
	}
}

func TestCreateConflictUsesWinner(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	a := newRing(t, ms, c, k1)
	cs := &counting{Store: ms}
	b := newRing(t, cs, c, k1)
	_, winner := mustEncrypt(t, a, "s", "v", "")
	cs.hidePrimary.Store(1) // b's first look misses the winner, so it tries to create and loses
	sealed, id := mustEncrypt(t, b, "s", "w", "")
	if id != winner {
		t.Fatalf("b used %s, want winner %s", keyring.FormatID(id), keyring.FormatID(winner))
	}
	if ks := keysOfScope(t, ms, "s"); len(ks) != 1 {
		t.Fatalf("scope keys = %+v", ks)
	}
	mustDecrypt(t, a, "s", sealed, "", "w")
}

// wrapAADOf repeats the package's id||scope binding of a wrapped DEK.
func wrapAADOf(k *keyring.DataKey) []byte {
	return append(append([]byte{}, k.ID[:]...), k.Scope...)
}

func TestCreateRejectsStoreMismatch(t *testing.T) {
	k1, k2, unheld := newKEK(t, 1), newKEK(t, 2), newKEK(t, 3)
	for name, st := range map[string]*lying{
		"other scope on read":  {onGet: func(k *keyring.DataKey) { k.Scope = "b" }},
		"KEK not held on read": {onGet: func(k *keyring.DataKey) { k.KEKID = unheld.ID() }},
		"truncated wrapped":    {onInsert: func(k *keyring.DataKey) { k.Wrapped = k.Wrapped[:len(k.Wrapped)-1] }},
		"altered wrapped":      {onInsert: func(k *keyring.DataKey) { k.Wrapped[len(k.Wrapped)-1] ^= 1 }},
		"wrapped other DEK":    {onInsert: func(k *keyring.DataKey) { k.Wrapped, _ = k1.Wrap(ctx, make([]byte, aead.KeySize), wrapAADOf(k)) }},
		"other DEK under held KEK": {onGet: func(k *keyring.DataKey) {
			k.KEKID = k2.ID()
			k.Wrapped, _ = k2.Wrap(ctx, make([]byte, aead.KeySize), wrapAADOf(k))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			rs := newRows()
			st.Store = rs.ms
			r := newRing(t, st, newClock(), k1, k2)
			calls := 0
			err := r.EncryptAndWrite(ctx, "a", []byte("v"), nil, func(sealed []byte, id [16]byte) error {
				calls++
				return rs.writer("x", "a")(sealed, id)
			})
			if !errors.Is(err, keyring.ErrStoreMismatch) || calls != 0 || len(rs.snapshot()) != 0 {
				t.Fatalf("err = %v after %d writes, want ErrStoreMismatch and none", err, calls)
			}
		})
	}
}

func TestCreateAcceptsConcurrentRewrap(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	// The re-read sees the new key already moved under k2, as after a rewrap between the insert and the read.
	st := &lying{Store: ms, onGet: func(k *keyring.DataKey) {
		if k.KEKID != k1.ID() {
			return
		}
		dek, err := k1.Unwrap(ctx, k.Wrapped, wrapAADOf(k))
		if err != nil {
			panic(err)
		}
		if k.Wrapped, err = k2.Wrap(ctx, dek, wrapAADOf(k)); err != nil {
			panic(err)
		}
		k.KEKID = k2.ID()
	}}
	r := newRing(t, st, c, k1, k2)
	sealed, id := mustEncrypt(t, r, "a", "v", "")
	if info, _ := keyInfo(r, id); info.KEKID != k2.ID() || info.Locked {
		t.Fatalf("key = %+v, want unlocked under k2", info)
	}
	mustDecrypt(t, r, "a", sealed, "", "v")
	mustDecrypt(t, newRing(t, ms, c, k1), "a", sealed, "", "v")
}

func TestCreateForOtherScopeDoesNotLendKey(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	st := &lying{Store: ms, onGet: func(k *keyring.DataKey) {
		if k.Scope == "a" {
			k.Scope = "b"
		}
	}}
	r := newRing(t, st, c, k1)
	if _, _, err := r.Encrypt(ctx, "a", []byte("v"), nil); !errors.Is(err, keyring.ErrStoreMismatch) {
		t.Fatalf("Encrypt: err = %v, want ErrStoreMismatch", err)
	}
	// A ring reading the store honestly encrypts under the key that was created.
	sealed, id := mustEncrypt(t, newRing(t, ms, c, k1), "a", "v", "")
	if ks := keysOfScope(t, ms, "a"); len(ks) != 1 || ks[0].ID != id {
		t.Fatalf("scope a keys = %+v, want only %s", ks, keyring.FormatID(id))
	}
	if info, ok := keyInfo(r, id); !ok || !info.Locked {
		t.Fatalf("key on the misled ring = %+v, want locked", info)
	}
	if _, _, err := r.Decrypt(ctx, "b", sealed, nil); !errors.Is(err, keyring.ErrKEKUnavailable) {
		t.Fatalf("Decrypt under the scope the store claimed: err = %v, want ErrKEKUnavailable", err)
	}
}

func TestCreateDemotedBeforeReadIsConflict(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	r := newRing(t, ms, c, k1)
	_, oldID := mustEncrypt(t, r, "a", "v", "")
	st := &lying{Store: ms, onGet: func(k *keyring.DataKey) {
		if k.ID != oldID {
			k.State = keyring.DecryptOnly
		}
	}}
	if _, err := newRing(t, st, c, k1).Rotate(ctx, "a"); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("Rotate: err = %v, want ErrConflict", err)
	}
}

func TestMissedRotateWriteRetries(t *testing.T) {
	c := newClock()
	rs := newRows()
	k1 := newKEK(t, 1)
	a := newRing(t, rs.ms, c, k1)
	b := newRing(t, rs.ms, c, k1)
	if err := b.EncryptAndWrite(ctx, "p", []byte("v1"), []byte("x"), rs.writer("x", "p")); err != nil {
		t.Fatal(err)
	}
	oldID := rs.get("x").keyID
	nk, err := a.Rotate(ctx, "p") // b is not told
	if err != nil {
		t.Fatal(err)
	}
	sealed, id := mustEncrypt(t, b, "p", "v2", "y")
	if id != oldID {
		t.Fatal("b should still believe the old key is Primary")
	}
	if err := rs.writer("y", "p")(sealed, id); !errors.Is(err, keyring.ErrNotPrimary) {
		t.Fatalf("write under demoted key: err = %v, want ErrNotPrimary", err)
	}
	if err := b.EncryptAndWrite(ctx, "p", []byte("v2"), []byte("y"), rs.writer("y", "p")); err != nil {
		t.Fatalf("EncryptAndWrite after missed rotate: %v", err)
	}
	if got := rs.get("y").keyID; got != nk.ID {
		t.Fatalf("row written under %s, want %s", keyring.FormatID(got), keyring.FormatID(nk.ID))
	}
	mustDecrypt(t, a, "p", rs.get("y").sealed, "y", "v2")
	if st := mustDecrypt(t, b, "p", rs.get("x").sealed, "x", "v1"); !st.Stale {
		t.Fatal("b: old row not stale after its reload")
	}
	// Other write errors are returned as is, without a retry.
	boom := errors.New("boom")
	calls := 0
	err = b.EncryptAndWrite(ctx, "p", []byte("v"), nil, func([]byte, [16]byte) error { calls++; return boom })
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
	// A second ErrNotPrimary is returned.
	calls = 0
	err = b.EncryptAndWrite(ctx, "p", []byte("v"), nil, func([]byte, [16]byte) error { calls++; return keyring.ErrNotPrimary })
	if !errors.Is(err, keyring.ErrNotPrimary) || calls != 2 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

func TestScopeMismatch(t *testing.T) {
	c := newClock()
	r := newRing(t, memstore.New(), c, newKEK(t, 1))
	a, _ := mustEncrypt(t, r, "a", "v", "row")
	empty, _ := mustEncrypt(t, r, "", "w", "row")
	mustDecrypt(t, r, "", empty, "row", "w")
	for _, tc := range []struct {
		sealed []byte
		scope  string
	}{{a, "b"}, {a, ""}, {empty, "a"}} {
		if _, _, err := r.Decrypt(ctx, tc.scope, tc.sealed, []byte("row")); !errors.Is(err, keyring.ErrScopeMismatch) {
			t.Fatalf("scope %q: err = %v, want ErrScopeMismatch", tc.scope, err)
		}
	}
	if _, _, err := r.Decrypt(ctx, "a", a, []byte("other-row")); !errors.Is(err, aead.ErrAuthFailed) {
		t.Fatalf("wrong aad: err = %v", err)
	}
}

func TestLockedPrimaryEncryptFails(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	early := newRing(t, ms, c, k2) // has never seen scope a
	sealed, _ := mustEncrypt(t, newRing(t, ms, c, k1), "a", "v", "")
	// The target moves to k2, but scope a's Primary stays under k1 (a rewrap that has not reached it).
	if err := ms.SetTargetKEK(ctx, k2.ID(), k1.ID()); err != nil {
		t.Fatal(err)
	}
	late := newRing(t, ms, c, k2)
	for name, r := range map[string]*keyring.Ring{"early": early, "late": late} {
		if _, _, err := r.Encrypt(ctx, "a", []byte("x"), nil); !errors.Is(err, keyring.ErrKEKUnavailable) {
			t.Fatalf("%s: Encrypt over locked Primary: %v", name, err)
		}
		if _, _, err := r.Decrypt(ctx, "a", sealed, nil); !errors.Is(err, keyring.ErrKEKUnavailable) {
			t.Fatalf("%s: Decrypt under locked key: %v", name, err)
		}
	}
	if ks := keysOfScope(t, ms, "a"); len(ks) != 1 {
		t.Fatalf("a key was created over the locked Primary: %+v", ks)
	}
	if ks := late.Keys(); len(ks) != 1 || !ks[0].Locked {
		t.Fatalf("Keys = %+v", ks)
	}
	mustEncrypt(t, late, "b", "v", "")
}

func TestLockedPrimaryUnlocksAfterMissedRewrap(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	both := newRing(t, ms, c, k1, k2)
	sealed, id := mustEncrypt(t, both, "a", "v", "")
	newOnly := newRing(t, ms, c, k2)
	if info, _ := keyInfo(newOnly, id); !info.Locked {
		t.Fatal("key under k1 not locked on a k2-only ring")
	}
	if _, err := both.Rewrap(ctx, k2.ID()); err != nil { // newOnly is not told
		t.Fatal(err)
	}
	if _, got := mustEncrypt(t, newOnly, "a", "w", ""); got != id {
		t.Fatalf("Encrypt used %s, want the rewrapped Primary %s", keyring.FormatID(got), keyring.FormatID(id))
	}
	mustDecrypt(t, newOnly, "a", sealed, "", "v")
}

type noLegacy struct{ kek.KEK }

func TestDecryptLegacy(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	key2 := make([]byte, 32)
	for i := range key2 {
		key2[i] = 2
	}
	block, _ := aes.NewCipher(key2)
	g, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	ct := g.Seal(nil, nonce, []byte("sk-old"), nil)

	// The opening KEK is second in the set, so every legacy-capable KEK is tried.
	got, st, err := newRing(t, ms, c, k1, newKEK(t, 2)).DecryptLegacy(ct, nonce)
	if err != nil || string(got) != "sk-old" || !st.Stale || st.Reason != keyring.StaleLegacy {
		t.Fatalf("DecryptLegacy = %q, %+v, %v", got, st, err)
	}
	if _, _, err := newRing(t, ms, c, k1).DecryptLegacy(ct, nonce); !errors.Is(err, aead.ErrAuthFailed) {
		t.Fatalf("without the key: %v", err)
	}
	if _, _, err := newRing(t, ms, c, noLegacy{k1}).DecryptLegacy(ct, nonce); !errors.Is(err, keyring.ErrKEKUnavailable) {
		t.Fatalf("no legacy-capable KEK: %v", err)
	}
}

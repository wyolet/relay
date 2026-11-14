package local

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/wyolet/relay/secret/aead"
	"github.com/wyolet/relay/secret/kek"
)

var (
	_ kek.KEK          = (*KEK)(nil)
	_ kek.LegacyOpener = (*KEK)(nil)
)

func mustNew(t testing.TB, key []byte) *KEK {
	t.Helper()
	k, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestIDGolden(t *testing.T) {
	seq := make([]byte, 32)
	for i := range seq {
		seq[i] = byte(i)
	}
	for _, c := range []struct {
		key  []byte
		want string
	}{
		{bytes.Repeat([]byte{0x42}, 32), "c5ad135c6f2738d2f0343438ae33ab3a"},
		{seq, "8a623e9eee31adbf529cd9c7d62c11c7"},
	} {
		k := mustNew(t, c.key)
		id := k.ID()
		if got := hex.EncodeToString(id[:]); got != c.want {
			t.Fatalf("ID = %s, want %s", got, c.want)
		}
		if again := mustNew(t, c.key).ID(); again != id {
			t.Fatal("ID not stable")
		}
		if bytes.Contains(c.key, id[:4]) {
			t.Fatal("ID carries key bytes")
		}
	}
}

func TestNewCopiesAndChecksKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if k, err := New(make([]byte, n)); err == nil || err.Error() != fmt.Sprintf("local: key must be 32 bytes, got %d", n) || k != nil {
			t.Fatalf("New(%d bytes) = %v, %v", n, k, err)
		}
	}
	key := bytes.Repeat([]byte{7}, 32)
	k := mustNew(t, key)
	wrapped, err := k.Wrap(context.Background(), []byte("dek"), nil)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 0xff
	if got, err := k.Unwrap(context.Background(), wrapped, nil); err != nil || string(got) != "dek" {
		t.Fatalf("Unwrap after caller mutated key: %q, %v", got, err)
	}
}

func TestWrapUnwrap(t *testing.T) {
	ctx := context.Background()
	k := mustNew(t, bytes.Repeat([]byte{1}, 32))
	dek := bytes.Repeat([]byte{0xd0}, 32)
	aad := append([]byte("0123456789abcdef"), "user:7"...)
	wrapped, err := k.Wrap(ctx, dek, aad)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := aead.KeyID(wrapped); err != nil || id != k.ID() {
		t.Fatalf("wrapped header id = %x, %v", id, err)
	}
	got, err := k.Unwrap(ctx, wrapped, aad)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("Unwrap = %x, %v", got, err)
	}
	// The wrap label is bound: a plain Seal with the same aad does not unwrap.
	plain, err := aead.Seal(bytes.Repeat([]byte{1}, 32), k.ID(), dek, aad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Unwrap(ctx, plain, aad); !errors.Is(err, aead.ErrAuthFailed) {
		t.Fatalf("unlabelled seal unwrapped: %v", err)
	}
}

func TestUnwrapAADBinding(t *testing.T) {
	ctx := context.Background()
	k := mustNew(t, bytes.Repeat([]byte{1}, 32))
	id1 := []byte("0123456789abcdef")
	id2 := []byte("0123456789abcdeX")
	wrapped, err := k.Wrap(ctx, []byte("dek"), append(append([]byte(nil), id1...), "user:7"...))
	if err != nil {
		t.Fatal(err)
	}
	for name, aad := range map[string][]byte{
		"other id":    append(append([]byte(nil), id2...), "user:7"...),
		"other scope": append(append([]byte(nil), id1...), "user:8"...),
		"empty scope": id1,
		"nil":         nil,
	} {
		if got, err := k.Unwrap(ctx, wrapped, aad); err != aead.ErrAuthFailed || got != nil {
			t.Fatalf("%s: Unwrap = %x, %v; want ErrAuthFailed", name, got, err)
		}
	}
}

func TestUnwrapRejects(t *testing.T) {
	ctx := context.Background()
	a := mustNew(t, bytes.Repeat([]byte{1}, 32))
	b := mustNew(t, bytes.Repeat([]byte{2}, 32))
	wrapped, err := a.Wrap(ctx, []byte("dek"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// b's id written over a's: the right KEK is named, but its key never sealed this.
	relabelled := bytes.Clone(wrapped)
	bID := b.ID()
	copy(relabelled[5:21], bID[:])
	flipped := bytes.Clone(wrapped)
	flipped[len(flipped)-1] ^= 1
	otherVersion := bytes.Clone(wrapped)
	otherVersion[4] = 2

	for _, tc := range []struct {
		name string
		k    *KEK
		in   []byte
		want error
	}{
		{"another KEK's value", b, wrapped, ErrWrongKEK},
		{"relabelled to this KEK", b, relabelled, aead.ErrAuthFailed},
		{"relabelled away from its KEK", a, relabelled, ErrWrongKEK},
		{"tag bit flipped", a, flipped, aead.ErrAuthFailed},
		{"truncated", a, wrapped[:len(wrapped)-1], aead.ErrAuthFailed},
		{"header only", a, wrapped[:21], aead.ErrNotSealed},
		{"unknown version", a, otherVersion, aead.ErrUnsupportedVersion},
		{"not sealed", a, []byte("not sealed"), aead.ErrNotSealed},
		{"empty", a, nil, aead.ErrNotSealed},
	} {
		if got, err := tc.k.Unwrap(ctx, tc.in, nil); err != tc.want || got != nil {
			t.Errorf("%s: Unwrap = %x, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestOpenLegacy(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{9}, g.NonceSize())
	ct := g.Seal(nil, nonce, []byte("sk-legacy"), nil)
	got, err := mustNew(t, key).OpenLegacy(ct, nonce)
	if err != nil || string(got) != "sk-legacy" {
		t.Fatalf("OpenLegacy = %q, %v", got, err)
	}
	if _, err := mustNew(t, bytes.Repeat([]byte{4}, 32)).OpenLegacy(ct, nonce); !errors.Is(err, aead.ErrAuthFailed) {
		t.Fatalf("other key: err = %v", err)
	}
}

func FuzzUnwrap(f *testing.F) {
	k := mustNew(f, bytes.Repeat([]byte{1}, 32))
	wrapped, err := k.Wrap(context.Background(), []byte("dek"), []byte("aad"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(wrapped, []byte("aad"))
	f.Add([]byte{}, []byte(nil))
	f.Add(wrapped[:21], []byte("aad"))
	f.Fuzz(func(t *testing.T, in, aad []byte) {
		got, err := k.Unwrap(context.Background(), in, aad)
		if err == nil && (!bytes.Equal(got, []byte("dek")) || !bytes.Equal(aad, []byte("aad"))) {
			t.Fatalf("Unwrap accepted forged input: %x", in)
		}
	})
}

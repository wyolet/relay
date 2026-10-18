package stored

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret/kek"
	"github.com/wyolet/relay/secret/kek/local"
	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

var testKey = bytes.Repeat([]byte{7}, 32)

func memRing(t *testing.T, key []byte) *keyring.Ring {
	t.Helper()
	k, err := local.New(key)
	if err != nil {
		t.Fatal(err)
	}
	set, err := kek.NewSet(k)
	if err != nil {
		t.Fatal(err)
	}
	r, err := keyring.New(context.Background(), memstore.New(), set, keyring.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// legacyWithMagic builds a legacy AES-GCM value whose ciphertext starts with the sealed format's magic and version byte, as about one legacy row in 2^32 does by chance.
func legacyWithMagic(t *testing.T, key []byte, size int) (plain, ct, nonce []byte) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce = bytes.Repeat([]byte{9}, gcm.NonceSize())
	stream := gcm.Seal(nil, nonce, make([]byte, size), nil)[:size]
	want := append([]byte{0x00, 'W', 'S', 'K', 0x01}, bytes.Repeat([]byte{'x'}, size-5)...)
	plain = make([]byte, size)
	for i := range plain {
		plain[i] = want[i] ^ stream[i]
	}
	return plain, gcm.Seal(nil, nonce, plain, nil), nonce
}

func TestOpenValue(t *testing.T) {
	ctx := context.Background()
	r := memRing(t, testKey)

	sealed, _, err := r.Encrypt(ctx, "user:u1", []byte("sk-1"), valueAAD("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got, st, err := openValue(ctx, r, "a", "user:u1", sealed, nil); err != nil || string(got) != "sk-1" || st.Stale {
		t.Fatalf("sealed: %q %+v %v", got, st, err)
	}
	if _, _, err := openValue(ctx, r, "b", "user:u1", sealed, nil); err == nil {
		t.Fatal("a value copied to another row opened")
	}
	if _, _, err := openValue(ctx, r, "a", "user:u2", sealed, nil); !errors.Is(err, keyring.ErrScopeMismatch) {
		t.Fatalf("another scope: %v, want ErrScopeMismatch", err)
	}

	ct, nonce, err := crypto.Encrypt(testKey, []byte("sk-legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if got, st, err := openValue(ctx, r, "c", "anything", ct, nonce); err != nil || string(got) != "sk-legacy" || st.Reason != keyring.StaleLegacy {
		t.Fatalf("legacy: %q %+v %v", got, st, err)
	}

	plain, magic, nonce := legacyWithMagic(t, testKey, 64)
	if !bytes.HasPrefix(magic, []byte{0x00, 'W', 'S', 'K'}) {
		t.Fatal("test value does not start with the magic")
	}
	got, st, err := openValue(ctx, r, "d", "system", magic, nonce)
	if err != nil || !bytes.Equal(got, plain) || st.Reason != keyring.StaleLegacy {
		t.Fatalf("legacy value starting with the magic: %+v %v", st, err)
	}
	if _, _, err := openValue(ctx, r, "d", "system", magic, nil); err == nil {
		t.Fatal("a value with the magic and no nonce opened as legacy")
	}
}

func TestRowScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     pgtype.Text
		hasOwner  bool
		kind, id  string
		want      string
		wantOwned bool
	}{
		{"recorded scope wins", pgtype.Text{String: "project:p", Valid: true}, true, "user", "u", "project:p", true},
		{"legacy user key", pgtype.Text{}, true, "user", "u", "user:u", true},
		{"legacy project key", pgtype.Text{}, true, "project", "p", "project:p", true},
		{"legacy system key", pgtype.Text{}, true, "system", "", "system", true},
		{"legacy key with no owner kind", pgtype.Text{}, true, "", "", "system", true},
		{"orphan", pgtype.Text{}, false, "", "", "system", false},
	} {
		got, owned := rowScope(tc.scope, tc.hasOwner, tc.kind, tc.id)
		if got != tc.want || owned != tc.wantOwned {
			t.Errorf("%s: (%q, %v), want (%q, %v)", tc.name, got, owned, tc.want, tc.wantOwned)
		}
	}
}

func TestWriteError(t *testing.T) {
	if err := writeError(&pgconn.PgError{Code: sqlstateNotPrimary, Message: "m"}); !errors.Is(err, keyring.ErrNotPrimary) {
		t.Fatalf("not primary: %v", err)
	}
	for _, code := range []string{sqlstateKeyMismatch, sqlstateLegacyWrite} {
		if err := writeError(&pgconn.PgError{Code: code}); err == nil || errors.Is(err, keyring.ErrNotPrimary) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	plain := errors.New("x")
	if writeError(plain) != plain {
		t.Fatal("unrelated error was rewritten")
	}
}

func TestScopeContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := scopeFrom(ctx); ok {
		t.Fatal("scope without WithScope")
	}
	if s, ok := scopeFrom(WithScope(ctx, "user:u")); !ok || s != "user:u" {
		t.Fatalf("scope = %q, %v", s, ok)
	}
}

func TestNewRefusesDuplicateMasterKeys(t *testing.T) {
	if _, err := New(nil, [][]byte{testKey, testKey}); err == nil {
		t.Fatal("the same master key twice was accepted")
	}
	if _, err := New(nil, [][]byte{testKey[:16]}); err == nil {
		t.Fatal("a short master key was accepted")
	}
	b, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Ring(context.Background()); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("no master key: %v", err)
	}
}

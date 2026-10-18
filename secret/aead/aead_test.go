package aead

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

var (
	testKey   = bytes.Repeat([]byte{0x42}, KeySize)
	testKeyID = [16]byte{0x01, 0x92, 0xf3, 0xa0, 0, 0, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0, 0xaa}
)

func mustSeal(t testing.TB, plaintext, aad []byte) []byte {
	t.Helper()
	sealed, err := Seal(testKey, testKeyID, plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 256, 64 << 10} {
		plain := make([]byte, n)
		for i := range plain {
			plain[i] = byte(i)
		}
		aad := []byte("table/row-7")
		sealed := mustSeal(t, plain, aad)
		if len(sealed) != n+overhead {
			t.Fatalf("len(sealed) = %d, want %d", len(sealed), n+overhead)
		}
		got, err := Open(testKey, sealed, aad)
		if err != nil {
			t.Fatalf("Open size=%d: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch size=%d", n)
		}
	}
}

func TestOverheadIs61(t *testing.T) {
	if overhead != 61 {
		t.Fatalf("overhead = %d, want 61", overhead)
	}
}

func TestSealLayout(t *testing.T) {
	sealed := mustSeal(t, []byte("hello"), nil)
	if !bytes.Equal(sealed[:5], []byte{0x00, 'W', 'S', 'K', 0x01}) {
		t.Fatalf("header prefix = %x", sealed[:5])
	}
	if !bytes.Equal(sealed[5:21], testKeyID[:]) {
		t.Fatalf("key id = %x", sealed[5:21])
	}
	again := mustSeal(t, []byte("hello"), nil)
	if bytes.Equal(sealed[21:45], again[21:45]) {
		t.Fatal("two seals reused a nonce")
	}
}

func TestNilAndEmptyAADAreEqual(t *testing.T) {
	sealed := mustSeal(t, []byte("x"), nil)
	if _, err := Open(testKey, sealed, []byte{}); err != nil {
		t.Fatalf("Open with empty aad: %v", err)
	}
}

func TestFlippedByteFails(t *testing.T) {
	aad := []byte("aad")
	sealed := mustSeal(t, []byte("a secret value"), aad)
	for i := range sealed {
		for _, bit := range []byte{0x01, 0x80} {
			tampered := bytes.Clone(sealed)
			tampered[i] ^= bit
			got, err := Open(testKey, tampered, aad)
			if err == nil {
				t.Fatalf("byte %d bit %#x: Open succeeded", i, bit)
			}
			if got != nil {
				t.Fatalf("byte %d: Open returned plaintext with error", i)
			}
			want := ErrAuthFailed
			switch {
			case i < 4:
				want = ErrNotSealed
			case i == 4:
				want = ErrUnsupportedVersion
			}
			if !errors.Is(err, want) {
				t.Fatalf("byte %d: err = %v, want %v", i, err, want)
			}
		}
	}
}

func TestWrongAADFails(t *testing.T) {
	sealed := mustSeal(t, []byte("v"), []byte("row-1"))
	for _, aad := range [][]byte{nil, []byte("row-2"), []byte("row-1 "), []byte("row-")} {
		if _, err := Open(testKey, sealed, aad); !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("aad %q: err = %v, want ErrAuthFailed", aad, err)
		}
	}
}

// The key id is authenticated: a header rewritten to name another key must not open.
func TestRewrittenKeyIDFails(t *testing.T) {
	sealed := mustSeal(t, []byte("v"), nil)
	sealed[5+keyIDSize-1] ^= 0xff
	if _, err := Open(testKey, sealed, nil); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	sealed := mustSeal(t, []byte("v"), nil)
	other := bytes.Repeat([]byte{0x99}, KeySize)
	if _, err := Open(other, sealed, nil); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestBadKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := Seal(make([]byte, n), testKeyID, []byte("v"), nil); err == nil {
			t.Fatalf("Seal with %d-byte key: want error", n)
		}
		if _, err := Open(make([]byte, n), mustSeal(t, []byte("v"), nil), nil); err == nil {
			t.Fatalf("Open with %d-byte key: want error", n)
		}
	}
}

func TestUnknownVersionFails(t *testing.T) {
	sealed := mustSeal(t, []byte("v"), nil)
	for _, v := range []byte{0x00, 0x02, 0xff} {
		sealed[4] = v
		if _, err := Open(testKey, sealed, nil); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("version %#x: err = %v, want ErrUnsupportedVersion", v, err)
		}
		if _, err := KeyID(sealed); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("version %#x: KeyID err = %v", v, err)
		}
		if !IsSealed(sealed) {
			t.Fatalf("version %#x: IsSealed = false", v)
		}
	}
}

func TestTruncatedFails(t *testing.T) {
	sealed := mustSeal(t, []byte("a secret value"), nil)
	for n := 0; n < len(sealed); n++ {
		if _, err := Open(testKey, sealed[:n], nil); err == nil {
			t.Fatalf("Open of %d/%d bytes succeeded", n, len(sealed))
		}
	}
}

func TestIsSealed(t *testing.T) {
	notSealed := map[string][]byte{
		"nil":          nil,
		"empty":        {},
		"json":         []byte(`{"access_token":"abc","refresh_token":"def","expiry":"2026-01-01T00:00:00Z"}`),
		"api key":      []byte("sk-test-not-a-real-key-0123456789abcdefghijklmnopqrstuvwxyz"),
		"magic only":   {0x00, 'W', 'S', 'K'},
		"wrong magic":  append([]byte{0x01, 'W', 'S', 'K', 0x01}, make([]byte, overhead)...),
		"text 64 long": bytes.Repeat([]byte("a"), 64),
	}
	for name, b := range notSealed {
		if IsSealed(b) {
			t.Errorf("IsSealed(%s) = true", name)
		}
	}
	if !IsSealed(mustSeal(t, nil, nil)) {
		t.Fatal("IsSealed(sealed empty plaintext) = false")
	}
	if !IsSealed([]byte{0x00, 'W', 'S', 'K', 0x02}) {
		t.Fatal("IsSealed(magic and a newer version) = false")
	}
}

func TestKeyID(t *testing.T) {
	id, err := KeyID(mustSeal(t, []byte("v"), nil))
	if err != nil || id != testKeyID {
		t.Fatalf("KeyID = %x, %v; want %x", id, err, testKeyID)
	}
	for _, b := range [][]byte{nil, []byte("sk-x"), {0x00, 'W', 'S', 'K', 0x01}} {
		if _, err := KeyID(b); !errors.Is(err, ErrNotSealed) {
			t.Fatalf("KeyID(%q): err = %v, want ErrNotSealed", b, err)
		}
	}
}

func TestParseKey(t *testing.T) {
	bad := []string{
		"",
		"!!!notbase64!!!",
		base64.StdEncoding.EncodeToString(make([]byte, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
		base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
	}
	for _, s := range bad {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q): want error", s)
		}
	}
	want := bytes.Repeat([]byte{7}, KeySize)
	got, err := ParseKey(base64.StdEncoding.EncodeToString(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ParseKey = %x, %v", got, err)
	}
}

func TestGenerateKey(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey(a)
	if err != nil || len(k) != KeySize {
		t.Fatalf("ParseKey(GenerateKey()) = %d bytes, %v", len(k), err)
	}
	b, _ := GenerateKey()
	if a == b {
		t.Fatal("GenerateKey returned the same key twice")
	}
}

func FuzzOpen(f *testing.F) {
	sealed := mustSeal(f, []byte("seed plaintext"), []byte("seed aad"))
	f.Add(sealed, []byte("seed aad"))
	f.Add(sealed[:overhead], []byte(nil))
	f.Add([]byte{}, []byte(nil))
	f.Add([]byte{0x00, 'W', 'S', 'K'}, []byte(nil))
	f.Add(append([]byte{0x00, 'W', 'S', 'K', 0x02}, make([]byte, overhead)...), []byte(nil))
	f.Add([]byte(`{"kind":"stored","id":"x"}`), []byte("x"))
	f.Fuzz(func(t *testing.T, sealed, aad []byte) {
		plain, err := Open(testKey, sealed, aad)
		if err == nil && !IsSealed(sealed) {
			t.Fatal("Open succeeded on input IsSealed rejects")
		}
		if err != nil && plain != nil {
			t.Fatal("Open returned plaintext with an error")
		}
		_, _ = KeyID(sealed)
		_, _ = OpenLegacy(testKey, sealed, aad)
	})
}

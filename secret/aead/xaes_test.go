package aead

import (
	"bytes"
	"crypto/sha3"
	"encoding/hex"
	"os"
	"testing"
)

// Vectors from https://c2sp.org/XAES-256-GCM.
func TestXAESVectors(t *testing.T) {
	cases := []struct {
		name, key, l, k1, kx, nx, plaintext, aad, ciphertext string
	}{
		{
			name:       "msb of L is 0",
			key:        "0101010101010101010101010101010101010101010101010101010101010101",
			l:          "7298caa565031eadc6ce23d23ea66378",
			k1:         "e531954aca063d5b8d9c47a47d4cc6f0",
			kx:         "c8612c9ed53fe43e8e005b828a1631a0bbcb6ab2f46514ec4f439fcfd0fa969b",
			nx:         "4d4e4f505152535455565758",
			plaintext:  "XAES-256-GCM",
			aad:        "",
			ciphertext: "ce546ef63c9cc60765923609b33a9a1974e96e52daf2fcf7075e2271",
		},
		{
			name:       "msb of L is 1",
			key:        "0303030303030303030303030303030303030303030303030303030303030303",
			l:          "91c08762876dccf9ba204a33768fa5fe",
			k1:         "23810ec50edb99f374409466ed1f4b7b",
			kx:         "e9c621d4cdd9b11b00a6427ad7e559aeedd66b3857646677748f8ca796cb3fd8",
			nx:         "4d4e4f505152535455565758",
			plaintext:  "XAES-256-GCM",
			aad:        "c2sp.org/XAES-256-GCM",
			ciphertext: "986ec1832593df5443a179437fd083bf3fdb41abd740a21f71eb769d",
		},
	}
	nonce := []byte("ABCDEFGHIJKLMNOPQRSTUVWX")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x, err := newXAES(mustHex(t, c.key))
			if err != nil {
				t.Fatal(err)
			}
			var l [16]byte
			x.block.Encrypt(l[:], l[:])
			if got := hex.EncodeToString(l[:]); got != c.l {
				t.Errorf("L = %s, want %s", got, c.l)
			}
			if got := hex.EncodeToString(x.k1[:]); got != c.k1 {
				t.Errorf("K1 = %s, want %s", got, c.k1)
			}
			kx, nx := x.derive(nonce)
			if got := hex.EncodeToString(kx); got != c.kx {
				t.Errorf("Kx = %s, want %s", got, c.kx)
			}
			if got := hex.EncodeToString(nx); got != c.nx {
				t.Errorf("Nx = %s, want %s", got, c.nx)
			}
			ct, err := x.seal(nil, nonce, []byte(c.plaintext), []byte(c.aad))
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(ct); got != c.ciphertext {
				t.Fatalf("ciphertext = %s, want %s", got, c.ciphertext)
			}
			pt, err := x.open(nil, nonce, ct, []byte(c.aad))
			if err != nil || string(pt) != c.plaintext {
				t.Fatalf("open = %q, %v", pt, err)
			}
		})
	}
}

// The C2SP accumulated randomized vector. The 1,000,000-iteration variant takes a while, so it runs only when XAES_LONG_TEST is set.
func TestXAESAccumulated(t *testing.T) {
	iterations, want := 10_000, "e6b9edf2df6cec60c8cbd864e2211b597fb69a529160cd040d56c0c210081939"
	if !testing.Short() && os.Getenv("XAES_LONG_TEST") != "" {
		iterations, want = 1_000_000, "2163ae1445985a30b60585ee67daa55674df06901b890593e824b8a7c885ab15"
	}
	rng, hash := sha3.NewSHAKE128(), sha3.NewSHAKE128()
	read := func(n int) []byte {
		b := make([]byte, n)
		if _, err := rng.Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	for i := 0; i < iterations; i++ {
		key, nonce := read(32), read(24)
		plaintext := read(int(read(1)[0]))
		aad := read(int(read(1)[0]))
		x, err := newXAES(key)
		if err != nil {
			t.Fatal(err)
		}
		ct, err := x.seal(nil, nonce, plaintext, aad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hash.Write(ct); err != nil {
			t.Fatal(err)
		}
		pt, err := x.open(nil, nonce, ct, aad)
		if err != nil || !bytes.Equal(pt, plaintext) {
			t.Fatalf("iteration %d: open = %x, %v", i, pt, err)
		}
	}
	sum := make([]byte, 32)
	if _, err := hash.Read(sum); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sum); got != want {
		t.Fatalf("accumulated hash after %d iterations = %s, want %s", iterations, got, want)
	}
}

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

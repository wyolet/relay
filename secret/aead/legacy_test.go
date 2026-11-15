package aead

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

type legacyVector struct {
	name              string
	nonce, ciphertext []byte
}

func readLegacyVectors(t *testing.T) []legacyVector {
	t.Helper()
	f, err := os.Open("testdata/legacy.txt")
	must(t, err)
	defer f.Close()
	var out []legacyVector
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("malformed vector line %q", line)
		}
		out = append(out, legacyVector{fields[0], mustHex(t, fields[1]), mustHex(t, fields[2])})
	}
	must(t, sc.Err())
	return out
}

func legacyTestKey() []byte {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func legacyPlaintext(name string) []byte {
	switch name {
	case "nil":
		return nil
	case "empty":
		return []byte{}
	case "short":
		return []byte("sk-test-legacy-0123456789")
	case "long":
		b := make([]byte, 4096)
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
		return b
	}
	return nil
}

func TestOpenLegacyGolden(t *testing.T) {
	vectors := readLegacyVectors(t)
	want := []string{"nil", "empty", "short", "long"}
	if len(vectors) != len(want) {
		t.Fatalf("got %d vectors, want %d", len(vectors), len(want))
	}
	key := legacyTestKey()
	for i, v := range vectors {
		if v.name != want[i] {
			t.Fatalf("vector %d is %q, want %q", i, v.name, want[i])
		}
		got, err := OpenLegacy(key, v.ciphertext, v.nonce)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if !bytes.Equal(got, legacyPlaintext(v.name)) {
			t.Fatalf("%s: plaintext mismatch", v.name)
		}
		if IsSealed(v.ciphertext) {
			t.Fatalf("%s: legacy ciphertext reads as sealed", v.name)
		}
	}
}

func TestOpenLegacyRejects(t *testing.T) {
	v := readLegacyVectors(t)[2]
	key := legacyTestKey()

	flip := func(b []byte, i int) []byte {
		out := bytes.Clone(b)
		out[i] ^= 1
		return out
	}
	for _, tc := range []struct {
		name                   string
		key, ciphertext, nonce []byte
		want                   string
	}{
		{"first ciphertext bit flipped", key, flip(v.ciphertext, 0), v.nonce, ErrAuthFailed.Error()},
		{"last tag bit flipped", key, flip(v.ciphertext, len(v.ciphertext)-1), v.nonce, ErrAuthFailed.Error()},
		{"nonce bit flipped", key, v.ciphertext, flip(v.nonce, 0), ErrAuthFailed.Error()},
		{"wrong key", flip(key, KeySize-1), v.ciphertext, v.nonce, ErrAuthFailed.Error()},
		{"tag truncated", key, v.ciphertext[:len(v.ciphertext)-1], v.nonce, ErrAuthFailed.Error()},
		{"shorter than the tag", key, v.ciphertext[:10], v.nonce, ErrAuthFailed.Error()},
		{"empty ciphertext", key, nil, v.nonce, ErrAuthFailed.Error()},
		{"16-byte key", key[:16], v.ciphertext, v.nonce, "aead: key must be 32 bytes, got 16"},
		{"11-byte nonce", key, v.ciphertext, v.nonce[:11], "aead: nonce must be 12 bytes, got 11"},
		{"24-byte nonce", key, v.ciphertext, make([]byte, nonceSize), "aead: nonce must be 12 bytes, got 24"},
	} {
		got, err := OpenLegacy(tc.key, tc.ciphertext, tc.nonce)
		if err == nil || err.Error() != tc.want || got != nil {
			t.Errorf("%s: OpenLegacy = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

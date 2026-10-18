package aead

import (
	"bufio"
	"bytes"
	"errors"
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
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

	tampered := bytes.Clone(v.ciphertext)
	tampered[0] ^= 0xff
	if _, err := OpenLegacy(key, tampered, v.nonce); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampered: err = %v, want ErrAuthFailed", err)
	}
	if _, err := OpenLegacy(bytes.Repeat([]byte{0x99}, KeySize), v.ciphertext, v.nonce); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("wrong key: err = %v, want ErrAuthFailed", err)
	}
	if _, err := OpenLegacy(key[:16], v.ciphertext, v.nonce); err == nil {
		t.Fatal("16-byte key: want error")
	}
	if _, err := OpenLegacy(key, v.ciphertext, v.nonce[:11]); err == nil {
		t.Fatal("11-byte nonce: want error")
	}
	if _, err := OpenLegacy(key, v.ciphertext[:10], v.nonce); err == nil {
		t.Fatal("ciphertext shorter than the tag: want error")
	}
}

package apikey

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	const prefix = "sk-test-"
	g, err := Generate(prefix)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body, ok := strings.CutPrefix(g.Plaintext, prefix)
	if !ok {
		t.Fatalf("Plaintext %q does not start with %q", g.Plaintext, prefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) != entropyBytes {
		t.Fatalf("body %q is not %d base64url bytes (err %v)", body, entropyBytes, err)
	}
	if len(g.Plaintext) != len(prefix)+64 {
		t.Errorf("Plaintext is %d chars, want %d", len(g.Plaintext), len(prefix)+64)
	}
	if g.Hash != Hash(g.Plaintext) {
		t.Errorf("Hash = %q, want Hash(Plaintext)", g.Hash)
	}
	if g.Prefix != g.Plaintext[:len(prefix)+displayChars] {
		t.Errorf("Prefix = %q, want the first %d chars", g.Prefix, len(prefix)+displayChars)
	}

	again, err := Generate(prefix)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if again.Plaintext == g.Plaintext {
		t.Error("two Generate calls returned the same key")
	}
}

func TestHash(t *testing.T) {
	// sha256("abc"), the FIPS 180-2 test vector.
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := Hash("abc"); got != want {
		t.Errorf("Hash(abc) = %q, want %q", got, want)
	}
}

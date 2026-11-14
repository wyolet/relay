package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/key"
)

// keygen prints a key, the hash a manifest stores for it, and its display prefix; the three must describe one key.
func TestRunKeygen(t *testing.T) {
	var err error
	out := captureStdout(t, func() { err = runKeygen(nil) })
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^plaintext: (\S+)\nkeyHash:   ([0-9a-f]{64})\nprefix:    (\S+)\n$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("output = %q, want the three labelled lines", out)
	}
	plaintext, hash, prefix := m[1], m[2], m[3]
	if sum := sha256.Sum256([]byte(plaintext)); hex.EncodeToString(sum[:]) != hash {
		t.Errorf("keyHash %s is not the SHA-256 of the printed plaintext", hash)
	}
	if !strings.HasPrefix(plaintext, key.TokenPrefix) || !strings.HasPrefix(plaintext, prefix) || len(prefix) <= len(key.TokenPrefix) || len(prefix) >= len(plaintext) {
		t.Errorf("plaintext %q prefix %q: want the plaintext to start with %q and the prefix to be a proper head of it", plaintext, prefix, key.TokenPrefix)
	}

	again := captureStdout(t, func() { err = runKeygen(nil) })
	if err != nil || again == out {
		t.Errorf("a second keygen printed the same key (err %v)", err)
	}
	only := captureStdout(t, func() { err = runKeygen([]string{"--prefix"}) })
	if err != nil || !regexp.MustCompile(`^`+regexp.QuoteMeta(key.TokenPrefix)+`\S+\n$`).MatchString(only) || strings.Contains(only, ":") {
		t.Errorf("--prefix printed %q (err %v), want one bare prefix", only, err)
	}
	if err := runKeygen([]string{"--nope"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

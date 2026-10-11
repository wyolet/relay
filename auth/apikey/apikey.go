// Package apikey generates opaque bearer keys and the hash a server stores in
// place of them. A key is the caller's prefix followed by 48 random bytes in
// unpadded base64url; the stored hash is the hex sha256 of the whole key.
//
// Storage, lookup, rotation and revocation are the caller's; so is the prefix,
// which only makes keys recognisable in logs and secret scanners.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// entropyBytes is the random-byte count fed into base64url: 64 characters,
// 384 bits of entropy.
const entropyBytes = 48

// displayChars is how many characters past the prefix the display prefix
// keeps: enough for a human to recognise the key in a list, short enough to
// surface in logs and UIs after creation.
const displayChars = 8

// Generated carries the result of Generate. Plaintext is returned to the
// holder exactly once and never persisted.
type Generated struct {
	Plaintext string
	Hash      string
	Prefix    string
}

// Generate produces a fresh key `<prefix><base64url(48 random bytes)>` with
// its Hash and display Prefix. Callers store Hash and Prefix and return
// Plaintext to the holder once.
func Generate(prefix string) (Generated, error) {
	buf := make([]byte, entropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return Generated{}, fmt.Errorf("apikey: read entropy: %w", err)
	}
	plaintext := prefix + base64.RawURLEncoding.EncodeToString(buf)
	display := plaintext
	if n := len(prefix) + displayChars; len(display) > n {
		display = display[:n]
	}
	return Generated{
		Plaintext: plaintext,
		Hash:      Hash(plaintext),
		Prefix:    display,
	}, nil
}

// Hash returns the stored form of a presented key: hex sha256 of the full
// plaintext, prefix included.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

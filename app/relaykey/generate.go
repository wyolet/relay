package relaykey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// TokenPrefix is the literal prefix every relay-issued bearer token
// starts with. The leading "sk-" matches the LLM-API convention so the
// token visually scans as a secret credential in logs.
const TokenPrefix = "sk-wr-"

// tokenEntropyBytes is the random-byte count fed into base64url. 48
// bytes → 64 base64url chars; combined with the prefix the issued
// token is 70 chars. 384 bits of entropy.
const tokenEntropyBytes = 48

// displayPrefixLen is how many leading chars of the plaintext token
// are kept as a non-secret display hint on Spec.Prefix. Long enough
// for a human to recognise the key in a list, short enough to be safe
// to surface in logs / UIs after creation.
const displayPrefixLen = len(TokenPrefix) + 8

// Generated carries the result of Generate. Plaintext is the only
// field the server ever sees in cleartext; it is returned to the
// caller exactly once and never persisted.
type Generated struct {
	Plaintext string
	KeyHash   string
	Prefix    string
}

// Generate produces a fresh relay-key plaintext and the derived
// KeyHash + display Prefix. The plaintext is `sk-wr-<base64url(48
// random bytes)>`. Callers store KeyHash + Prefix and return
// Plaintext to the user once.
func Generate() (Generated, error) {
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return Generated{}, fmt.Errorf("relaykey.Generate: read entropy: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	plaintext := TokenPrefix + body
	sum := sha256.Sum256([]byte(plaintext))
	prefix := plaintext
	if len(prefix) > displayPrefixLen {
		prefix = prefix[:displayPrefixLen]
	}
	return Generated{
		Plaintext: plaintext,
		KeyHash:   hex.EncodeToString(sum[:]),
		Prefix:    prefix,
	}, nil
}

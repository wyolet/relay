package kv

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// redactKey returns a form of key safe to log or put in an error: keys can embed credentials (session tokens), so only the {tag} hash tag survives, and an untagged key is reduced to a short sha256 prefix.
func redactKey(key string) string {
	if strings.HasPrefix(key, "{") {
		if end := strings.IndexByte(key, '}'); end > 0 {
			return key[:end+1]
		}
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

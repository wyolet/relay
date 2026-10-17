package session

import (
	"crypto/sha256"
	"encoding/hex"
)

// No script or lock spans these keys, so each is tagged by its own scope: one tag per session spreads sessions over every slot, and one per user keeps a user's index entries on one slot for DestroyUser's Range. No key holds a token, only its sha256: kv keeps a key's {tag} in logs and errors, and a key listing must not hand out live sessions. Both are written with the session lifetime as TTL.

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sessionKey(token string) string { return "{sess:" + tokenHash(token) + "}:data" }

// userIndexPrefix covers every index entry of one user.
func userIndexPrefix(userID string) string { return "{sessuser:" + userID + "}:" }

func userIndexKey(userID, token string) string {
	return userIndexPrefix(userID) + tokenHash(token)
}

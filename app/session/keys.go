package session

// Keys carry no {tag}: hash tag: no script or lock spans them, and sessions issued before a deploy must resolve after it. Both are written with the session lifetime as TTL.

func sessionKey(prefix, token string) string { return prefix + token }

// userIndexPrefix covers every index entry of one user. Session tokens are base64url, so the "user:" segment never collides with a session key.
func userIndexPrefix(prefix, userID string) string { return prefix + "user:" + userID + ":" }

func userIndexKey(prefix, userID, token string) string {
	return userIndexPrefix(prefix, userID) + token
}

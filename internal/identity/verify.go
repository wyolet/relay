package identity

import (
	"log/slog"

	"github.com/wyolet/relay/auth/password"
)

// Verify checks whether the supplied cleartext matches user's stored
// password: a bcrypt hash, or legacy plain cleartext compared in constant
// time. A plain value logs a one-line deprecation warning per verification
// so operators see the nudge.
//
// Returns false on any mismatch or error (including "not resolved" — Verify
// is a request-time call, never panic).
func Verify(user *User, cleartext string) bool {
	if user == nil {
		return false
	}
	defer func() { _ = recover() }() // SecretRef.Get panics if unresolved

	ok, plain := password.Verify(user.Spec.Password.Get(), cleartext)
	if plain {
		slog.Warn("identity: user has plain-text password in YAML; switch to bcrypt hash",
			"user", user.Metadata.Name, "source", user.Spec.Password.Source())
	}
	return ok
}

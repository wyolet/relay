package identity

import (
	"crypto/subtle"
	"log/slog"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Verify checks whether the supplied cleartext password matches user's
// stored password. Two formats are accepted:
//
//   - bcrypt — value starts with "$2a$", "$2b$" or "$2y$". Compared via
//     bcrypt.CompareHashAndPassword in constant time.
//   - plain cleartext — anything else. Compared in constant time. This is
//     the legacy YAML format; we log a one-line deprecation warning per
//     verification so operators see the nudge.
//
// Returns false on any mismatch or error (including "not resolved" — Verify
// is a request-time call, never panic).
func Verify(user *User, password string) bool {
	if user == nil {
		return false
	}
	defer func() { _ = recover() }() // SecretRef.Get panics if unresolved

	stored := user.Spec.Password.Get()
	if stored == "" || password == "" {
		return false
	}

	if isBcryptHash(stored) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil
	}

	slog.Warn("identity: user has plain-text password in YAML; switch to bcrypt hash",
		"user", user.Metadata.Name, "source", user.Spec.Password.Source())
	return subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1
}

func isBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") ||
		strings.HasPrefix(s, "$2b$") ||
		strings.HasPrefix(s, "$2y$")
}

// Package password hashes and verifies login passwords with bcrypt, keeps a
// constant-time fallback for stored values that are not bcrypt hashes, and
// recognises published placeholder passwords.
//
// Where the stored value comes from, what to do about a plain-text one, and
// any login throttling are the caller's.
package password

import (
	"crypto/subtle"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Verify reports whether password matches stored. A bcrypt hash ("$2a$",
// "$2b$" or "$2y$") is compared with bcrypt; anything else is a legacy plain
// value compared in constant time, and plain reports that so the caller can
// flag it. An empty stored value or password never matches.
func Verify(stored, password string) (ok, plain bool) {
	if stored == "" || password == "" {
		return false, false
	}
	if IsBcryptHash(stored) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil, false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1, true
}

// Hash bcrypt-hashes password at the default cost.
func Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// IsBcryptHash reports whether s carries a bcrypt hash prefix.
func IsBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") ||
		strings.HasPrefix(s, "$2b$") ||
		strings.HasPrefix(s, "$2y$")
}

// placeholders are values published as example or default admin passwords;
// anyone can guess them, so they never authenticate.
var placeholders = map[string]struct{}{
	"change-me-please": {},
	"change-me":        {},
	"changeme":         {},
	"password":         {},
}

// IsPlaceholder reports whether password is a published placeholder,
// case-insensitively.
func IsPlaceholder(password string) bool {
	_, ok := placeholders[strings.ToLower(password)]
	return ok
}

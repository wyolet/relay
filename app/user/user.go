// Package user owns the DB-backed user account: the row behind a login
// session. Users are not a catalog kind — no metadata envelope, no snapshot,
// no NOTIFY; they are identity, not routing config.
//
// Two credential shapes coexist on one row: a local bcrypt password hash
// (password login) and/or an external identity subject ("issuer|sub", OIDC
// login). Either may be absent. Authorization is out of scope — roles are
// carried verbatim for app/authz to interpret.
package user

import (
	"crypto/subtle"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// RoleAdmin is the only role with defined meaning today: full access,
// including rows owned by other users.
const RoleAdmin = "admin"

// User is one account row.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email,omitempty"`
	PasswordHash string    `json:"-"`
	OIDCSubject  string    `json:"oidcSubject,omitempty"`
	Roles        []string  `json:"roles,omitempty"`
	Disabled     bool      `json:"disabled,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

// HasRole reports whether the user carries role.
func (u *User) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// VerifyPassword checks cleartext against the stored hash. bcrypt hashes
// ("$2a$"/"$2b$"/"$2y$" prefix) compare via bcrypt; anything else is treated
// as a legacy plain value and compared constant-time (the YAML seed hashes
// plain passwords on ingest, so plain values here should not occur — kept
// for defense in depth). Empty hash never matches: an OIDC-only user has no
// password login.
func VerifyPassword(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	if isBcrypt(hash) {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(password)) == 1
}

// HashPassword bcrypt-hashes a cleartext password for storage.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func isBcrypt(s string) bool {
	return strings.HasPrefix(s, "$2a$") ||
		strings.HasPrefix(s, "$2b$") ||
		strings.HasPrefix(s, "$2y$")
}

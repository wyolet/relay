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
	"time"

	"github.com/wyolet/relay/auth/password"
)

// RoleAdmin is the only role with defined meaning today: full access,
// including rows owned by other users.
const RoleAdmin = "admin"

// User is one account row.
type User struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	Email        string   `json:"email,omitempty"`
	PasswordHash string   `json:"-"`
	OIDCSubject  string   `json:"oidcSubject,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Disabled     bool     `json:"disabled,omitempty"`

	// TokenVersion is the generation every inference token this user holds
	// must carry. Bumping it invalidates all of them at once.
	TokenVersion int `json:"tokenVersion,omitempty"`

	CreatedAt time.Time `json:"createdAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
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

// VerifyPassword checks cleartext against the stored hash. A plain stored
// value is accepted without a warning: the YAML seed hashes plain passwords
// on ingest, so one here should not occur and is kept for defense in depth.
// Empty hash never matches: an OIDC-only user has no password login.
func VerifyPassword(hash, cleartext string) bool {
	ok, _ := password.Verify(hash, cleartext)
	return ok
}

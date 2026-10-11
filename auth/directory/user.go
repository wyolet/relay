package directory

import (
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/auth/password"
	"github.com/wyolet/relay/auth/principal"
)

// IdentitySeparator joins an identity's issuer and subject in its stored form; an issuer must not contain it.
const IdentitySeparator = "|"

// Identity is the stable identity an external identity provider asserts for a user.
type Identity struct {
	Issuer  string
	Subject string
}

// IsZero reports whether the identity is unset, as for a local-only account.
func (i Identity) IsZero() bool { return i.Issuer == "" && i.Subject == "" }

// User is an account. It carries no metadata envelope: its fields are its columns.
type User struct {
	// ID is the account's durable id; a create mints a UUIDv7 when it is empty.
	ID       string
	Username string
	Email    string
	// PasswordHash is a password hash, never a password; empty means no password login.
	PasswordHash string
	// Identity is the external identity the account signs in with; zero for none.
	Identity Identity
	// Roles are role names the account holds directly. Changing them goes through the grant check.
	Roles    []string
	Disabled bool
	// TokenVersion is the generation every token the user holds must carry. The store owns it: an upsert leaves it alone and BumpTokenVersion advances it.
	TokenVersion int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Status is the account state a principal for this user carries.
func (u *User) Status() principal.Status {
	if u.Disabled {
		return principal.StatusDisabled
	}
	return principal.StatusActive
}

func (u *User) validate() error {
	if strings.TrimSpace(u.Username) == "" {
		return fmt.Errorf("%w: user: username is required", ErrInvalid)
	}
	if (u.Identity.Issuer == "") != (u.Identity.Subject == "") {
		return fmt.Errorf("%w: user %q: identity needs both issuer and subject", ErrInvalid, u.Username)
	}
	if strings.Contains(u.Identity.Issuer, IdentitySeparator) {
		return fmt.Errorf("%w: user %q: identity issuer must not contain %q", ErrInvalid, u.Username, IdentitySeparator)
	}
	return nil
}

// validatePasswordHash refuses a value that is not a hash. Only a newly written value is checked, so a legacy stored value survives an unrelated edit.
func validatePasswordHash(u *User) error {
	if u.PasswordHash != "" && !password.IsBcryptHash(u.PasswordHash) {
		return fmt.Errorf("%w: user %q: password hash is not a bcrypt hash", ErrInvalid, u.Username)
	}
	return nil
}

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

// CanonicalIssuer is issuer without trailing slashes, the form identities are stored and looked up in: a provider and a configuration that disagree only on the slash name one issuer.
func CanonicalIssuer(issuer string) string { return strings.TrimRight(issuer, "/") }

// Canonical is i with its issuer in canonical form; two identities with the same canonical form are one identity.
func (i Identity) Canonical() Identity {
	i.Issuer = CanonicalIssuer(i.Issuer)
	return i
}

func (i Identity) validate(username string) error {
	if (i.Issuer == "") != (i.Subject == "") {
		return fmt.Errorf("%w: user %q: identity needs both issuer and subject", ErrInvalid, username)
	}
	if strings.Contains(i.Issuer, IdentitySeparator) {
		return fmt.Errorf("%w: user %q: identity issuer must not contain %q", ErrInvalid, username, IdentitySeparator)
	}
	return nil
}

// UserIdentity is one identity a user signs in with.
type UserIdentity struct {
	Identity
	// ExternalID is the provider's own id for the user behind this identity, such as an object id behind a pairwise subject; empty for none. It is unique per issuer.
	ExternalID string
	// CreatedAt is set by the store.
	CreatedAt time.Time
}

// User is an account. It carries no metadata envelope: its fields are its columns.
type User struct {
	// ID is the account's durable id; a create mints a UUIDv7 when it is empty.
	ID       string
	Username string
	Email    string
	// PasswordHash is a password hash, never a password; empty means no password login.
	PasswordHash string
	// Identity is the account's primary identity; zero only for an account with no identities. Writing a different one replaces the primary in Identities; writing a zero one removes it and makes the oldest remaining identity primary. Its issuer is kept as written, which a reader matching it exactly still finds.
	Identity Identity
	// Identities are every identity the account signs in with, oldest first, the primary among them, each in canonical form. The store fills them on read and an upsert leaves them alone apart from the primary; Directory.AddIdentity and RemoveIdentity change them.
	Identities []UserIdentity
	// External is the provisioning source that created the account and its id there. An update that leaves it zero keeps the stored one.
	External External
	// Roles are role names the account holds directly. Changing them goes through the grant check.
	Roles    []string
	Disabled bool
	// TokenVersion is the generation every token the user holds must carry. The store owns it: BumpTokenVersion advances it, and so does every write that changes the password, identities or disabled flag.
	TokenVersion int
	// ResourceVersion changes on every write to the account. An update carrying it applies only while the stored account still has it; empty updates unconditionally.
	ResourceVersion string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ExpectedVersion is Metadata.ExpectedVersion for an account.
func (u *User) ExpectedVersion() (v int64, ok bool, err error) {
	return Metadata{ResourceVersion: u.ResourceVersion}.ExpectedVersion()
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
	if err := u.Identity.validate(u.Username); err != nil {
		return err
	}
	return u.External.validate("user", u.Username)
}

// HasIdentity reports whether id, with or without trailing slashes on its issuer, is one of the account's identities.
func (u *User) HasIdentity(id Identity) bool {
	id = id.Canonical()
	for _, x := range u.Identities {
		if x.Canonical() == id {
			return true
		}
	}
	return false
}

// validatePasswordHash refuses a value that is not a hash. Only a newly written value is checked, so a legacy stored value survives an unrelated edit.
func validatePasswordHash(u *User) error {
	if u.PasswordHash != "" && !password.IsBcryptHash(u.PasswordHash) {
		return fmt.Errorf("%w: user %q: password hash is not a bcrypt hash", ErrInvalid, u.Username)
	}
	return nil
}

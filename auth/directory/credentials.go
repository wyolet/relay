package directory

import (
	"fmt"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/principal"
)

// ChangesCredentials reports whether writing u over stored, nil for a new account, sets, changes or clears its password or primary sign-in identity. Either lets whoever writes it sign in as the account.
func ChangesCredentials(stored, u *User) bool {
	if stored == nil {
		return u.PasswordHash != "" || !u.Identity.IsZero()
	}
	return u.PasswordHash != stored.PasswordHash || u.Identity != stored.Identity
}

// checkCredentials refuses a credential change from anyone but an admin. A nil principal on a create is the product registering an account itself, as a first sign-in does.
func checkCredentials(p *principal.Principal, stored, u *User) error {
	if !ChangesCredentials(stored, u) || p != nil && p.Admin || stored == nil && p == nil {
		return nil
	}
	return fmt.Errorf("%w: user %q: setting a password or sign-in identity requires an admin", authz.ErrForbidden, u.Username)
}

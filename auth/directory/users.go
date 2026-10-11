package directory

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/principal"
)

// CreateUser adds an account, minting its id when it has none. Roles on a new account go through the grant check like any role change; an account with no roles needs no principal, so p may be nil for self-registration. A non-nil p must be an admin to create an account carrying a password or identity.
func (d *Directory) CreateUser(ctx context.Context, p *principal.Principal, u *User) error {
	if err := u.validate(); err != nil {
		return err
	}
	if err := validatePasswordHash(u); err != nil {
		return err
	}
	if err := checkCredentials(p, nil, u); err != nil {
		return err
	}
	users := d.cfg.Stores.Users
	if u.ID == "" {
		u.ID = NewID()
	} else if existing, err := users.Get(ctx, u.ID); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("%w: user id %q is taken", ErrConflict, u.ID)
	}
	if err := d.checkUserUnique(ctx, u); err != nil {
		return err
	}
	if err := d.checkUserRoles(ctx, p, nil, u.Roles); err != nil {
		return err
	}
	if err := users.Upsert(ctx, u); err != nil {
		return err
	}
	d.notify(ctx, KindUser, OpUpsert, u.ID)
	return nil
}

// UpdateUser replaces the account with id u.ID; a zero u.External keeps the stored one. The write is conditional on u.ResourceVersion, or on the version read here when it is empty, so a concurrent change fails with ErrStaleResourceVersion instead of being lost. A role change goes through the grant check for every role added or removed and ends the user's sessions, which carry the roles they logged in with. Changing the password or primary identity needs an admin p. Disabling, re-enabling or changing the password or primary identity bumps the token version in the same write, so no token minted before survives, and ends the sessions. u.TokenVersion is not refreshed and u.ResourceVersion is cleared; read the user back for them.
func (d *Directory) UpdateUser(ctx context.Context, p *principal.Principal, u *User) error {
	users := d.cfg.Stores.Users
	stored, err := users.Get(ctx, u.ID)
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, u.ID)
	}
	if u.ResourceVersion != "" && u.ResourceVersion != stored.ResourceVersion {
		return ErrStaleResourceVersion
	}
	if err := u.validate(); err != nil {
		return err
	}
	if u.PasswordHash != stored.PasswordHash {
		if err := validatePasswordHash(u); err != nil {
			return err
		}
	}
	if err := checkCredentials(p, stored, u); err != nil {
		return err
	}
	if err := d.checkUserUnique(ctx, u); err != nil {
		return err
	}
	rolesChanged := !slices.Equal(stored.Roles, u.Roles)
	if rolesChanged {
		if err := d.checkUserRoles(ctx, p, stored.Roles, u.Roles); err != nil {
			return err
		}
	}
	u.TokenVersion, u.CreatedAt = stored.TokenVersion, stored.CreatedAt
	if u.External.IsZero() {
		u.External = stored.External
	}
	credentialsChanged := ChangesCredentials(stored, u)
	// The write applies only while the account is as read here, so a concurrent change is a conflict, not overwritten.
	u.ResourceVersion = stored.ResourceVersion
	err = users.Upsert(ctx, u)
	u.ResourceVersion = ""
	if err != nil {
		return err
	}
	return d.afterUserWrite(ctx, u, stored.Disabled, rolesChanged || credentialsChanged)
}

// DisableUser disables the account: its token version is bumped and its sessions end. Disabling a disabled account does nothing.
func (d *Directory) DisableUser(ctx context.Context, id string) error {
	return d.setDisabled(ctx, id, true)
}

// EnableUser re-enables the account. The token version is bumped in the same write, so a token minted before the account was disabled stays dead.
func (d *Directory) EnableUser(ctx context.Context, id string) error {
	return d.setDisabled(ctx, id, false)
}

// setDisabled writes the flag alone, so no other field of a stale read is written back.
func (d *Directory) setDisabled(ctx context.Context, id string, disabled bool) error {
	users := d.cfg.Stores.Users
	u, err := users.Get(ctx, id)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	if u.Disabled == disabled {
		return nil
	}
	changed, err := users.SetDisabled(ctx, id, disabled)
	if err != nil || !changed {
		// Unchanged means a concurrent write got there first and ended the sessions itself.
		return err
	}
	if err := d.revoke(ctx, id); err != nil {
		return err
	}
	op := OpUpsert
	if disabled {
		op = OpDisable
	}
	d.notify(ctx, KindUser, op, id)
	return nil
}

// afterUserWrite ends sessions when the account's state, roles or credentials moved, and reports the write. The store's write already bumped the token version.
func (d *Directory) afterUserWrite(ctx context.Context, u *User, wasDisabled, endSessions bool) error {
	if u.Disabled != wasDisabled || endSessions {
		if err := d.revoke(ctx, u.ID); err != nil {
			return err
		}
	}
	op := OpUpsert
	if u.Disabled && !wasDisabled {
		op = OpDisable
	}
	d.notify(ctx, KindUser, op, u.ID)
	return nil
}

// DeleteUser removes the account; its store drops it from every group and binding, and its sessions end.
func (d *Directory) DeleteUser(ctx context.Context, id string) error {
	users := d.cfg.Stores.Users
	u, err := users.Get(ctx, id)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	if err := users.Delete(ctx, id); err != nil {
		return err
	}
	if err := d.revoke(ctx, id); err != nil {
		return err
	}
	d.notify(ctx, KindUser, OpDelete, id)
	return nil
}

// checkUserUnique refuses a username or identity another account holds, ahead of the store's own constraint, so the error names the field.
func (d *Directory) checkUserUnique(ctx context.Context, u *User) error {
	users := d.cfg.Stores.Users
	other, err := users.ByUsername(ctx, u.Username)
	if err != nil {
		return err
	}
	if other != nil && other.ID != u.ID {
		return fmt.Errorf("%w: username %q is taken", ErrConflict, u.Username)
	}
	if u.Identity.IsZero() {
		return nil
	}
	other, err = users.ByIdentity(ctx, u.Identity)
	if err != nil {
		return err
	}
	if other != nil && other.ID != u.ID {
		return fmt.Errorf("%w: identity %s%s%s belongs to another user", ErrConflict, u.Identity.Issuer, IdentitySeparator, u.Identity.Subject)
	}
	return nil
}

// checkUserRoles runs the grant check at the global scope for every role added or removed: removing a role the caller does not hold would let it demote a stronger account. A name with no role row behind it has no rules to check, so only an admin may hand it out or take it away.
func (d *Directory) checkUserRoles(ctx context.Context, p *principal.Principal, before, after []string) error {
	changed := roleDiff(before, after)
	if len(changed) == 0 {
		return nil
	}
	roles, err := d.cfg.Stores.Roles.List(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]*Role, len(roles))
	for _, r := range roles {
		byName[r.Meta.Name] = r
	}
	for _, name := range changed {
		if r, ok := byName[name]; ok {
			if err := d.checkGrant(ctx, p, r, Global); err != nil {
				return err
			}
			continue
		}
		if !p.Authenticated() {
			return authz.ErrUnauthenticated
		}
		if !p.Admin {
			return fmt.Errorf("%w: role %q is not a stored role; changing it requires an admin", authz.ErrForbidden, name)
		}
	}
	return nil
}

// roleDiff returns the names in exactly one of a and b, in order of first appearance.
func roleDiff(a, b []string) []string {
	var out []string
	for _, n := range a {
		if !slices.Contains(b, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, n := range b {
		if !slices.Contains(a, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

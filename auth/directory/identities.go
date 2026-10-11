package directory

import (
	"context"
	"fmt"
)

// AddIdentity links an identity to the account, or updates its external id when the account already holds it. An identity another account holds is ErrConflict. The first identity of an account with none becomes its primary.
func (d *Directory) AddIdentity(ctx context.Context, userID string, id UserIdentity) error {
	users := d.cfg.Stores.Users
	u, err := users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if id.IsZero() {
		return fmt.Errorf("%w: user %q: identity needs both issuer and subject", ErrInvalid, u.Username)
	}
	if err := id.Identity.validate(u.Username); err != nil {
		return err
	}
	other, err := users.ByIdentity(ctx, id.Identity)
	if err != nil {
		return err
	}
	if other != nil && other.ID != userID {
		return fmt.Errorf("%w: identity %s%s%s belongs to another user", ErrConflict, id.Issuer, IdentitySeparator, id.Subject)
	}
	if err := users.AddIdentity(ctx, userID, id); err != nil {
		return err
	}
	d.notify(ctx, KindUser, OpUpsert, userID)
	return nil
}

// UnlinkedUsersByExternalID returns the accounts a provisioning source created with externalID that hold no identity yet, oldest first: the accounts a product may link a first sign-in to. Two sources may hold the same id, so more than one may come back.
func (d *Directory) UnlinkedUsersByExternalID(ctx context.Context, externalID string) ([]*User, error) {
	if externalID == "" {
		return nil, nil
	}
	all, err := d.cfg.Stores.Users.ByExternalID(ctx, externalID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, u := range all {
		if u.External.Source != "" && u.Identity.IsZero() && len(u.Identities) == 0 {
			out = append(out, u)
		}
	}
	return out, nil
}

// AddFirstIdentity links id to an account that holds no identity, as its primary. The store checks and writes in one step, so of two concurrent first links to one account only one succeeds; the other, like a link to an account that already has an identity, is ErrConflict.
func (d *Directory) AddFirstIdentity(ctx context.Context, userID string, id Identity) error {
	users := d.cfg.Stores.Users
	u, err := users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if id.Issuer == "" || id.Subject == "" {
		return fmt.Errorf("%w: user %q: identity needs both issuer and subject", ErrInvalid, u.Username)
	}
	if err := id.validate(u.Username); err != nil {
		return err
	}
	if err := users.AddFirstIdentity(ctx, userID, id); err != nil {
		return err
	}
	d.notify(ctx, KindUser, OpUpsert, userID)
	return nil
}

// RemoveIdentities unlinks every identity of the account in one write and ends its sessions; an account with none is left alone. An account without a password keeps them, so it keeps a way to sign in.
func (d *Directory) RemoveIdentities(ctx context.Context, userID string) error {
	users := d.cfg.Stores.Users
	u, err := users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if len(u.Identities) == 0 {
		return nil
	}
	if u.PasswordHash == "" {
		return fmt.Errorf("%w: user %q: an account without a password cannot have every identity removed", ErrInvalid, u.Username)
	}
	if err := users.RemoveIdentities(ctx, userID); err != nil {
		return err
	}
	if err := d.revoke(ctx, userID); err != nil {
		return err
	}
	d.notify(ctx, KindUser, OpUpsert, userID)
	return nil
}

// RemoveIdentity unlinks an identity from the account and ends its sessions, any of which may have signed in with it. The last identity of an account without a password stays, so the account keeps a way to sign in. Removing the primary makes the oldest remaining identity primary.
func (d *Directory) RemoveIdentity(ctx context.Context, userID string, id Identity) error {
	users := d.cfg.Stores.Users
	u, err := users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if !u.HasIdentity(id) {
		return fmt.Errorf("%w: user %q has no identity %s%s%s", ErrNotFound, u.Username, id.Issuer, IdentitySeparator, id.Subject)
	}
	if len(u.Identities) == 1 && u.PasswordHash == "" {
		return fmt.Errorf("%w: user %q: the last identity of an account without a password cannot be removed", ErrInvalid, u.Username)
	}
	if err := users.RemoveIdentity(ctx, userID, id); err != nil {
		return err
	}
	if err := d.revoke(ctx, userID); err != nil {
		return err
	}
	d.notify(ctx, KindUser, OpUpsert, userID)
	return nil
}

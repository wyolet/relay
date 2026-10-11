package memstore

import (
	"context"
	"fmt"
	"strconv"

	"github.com/wyolet/relay/auth/directory"
)

func (u userStore) Upsert(_ context.Context, in *directory.User) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	return u.upsert(in, false)
}

func (u userStore) UpsertRemovingIdentities(_ context.Context, in *directory.User) error {
	if !in.Identity.IsZero() {
		return fmt.Errorf("%w: user %q: removing every identity needs a zero primary", directory.ErrInvalid, in.Username)
	}
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	return u.upsert(in, true)
}

// upsert is Upsert under the lock; removeIdentities drops every identity in the same write instead of promoting one.
func (u userStore) upsert(in *directory.User, removeIdentities bool) error {
	want, conditional, err := in.ExpectedVersion()
	if err != nil {
		return err
	}
	stored := u.s.users[in.ID]
	if conditional && stored != nil && stored.ResourceVersion != directory.FormatResourceVersion(want) {
		return directory.ErrStaleResourceVersion
	}
	if err := u.checkUnique(in.ID, in.Username, in.Email, in.External); err != nil {
		return err
	}
	for id, x := range u.s.users {
		if id != in.ID && !in.Identity.IsZero() && x.HasIdentity(in.Identity) {
			return fmt.Errorf("%w: identity is taken", directory.ErrConflict)
		}
	}
	c := cloneUser(in)
	c.TokenVersion, c.CreatedAt, c.Identities, c.ResourceVersion = 1, now(), nil, "0"
	if stored != nil {
		c.TokenVersion, c.CreatedAt, c.ResourceVersion = stored.TokenVersion, stored.CreatedAt, stored.ResourceVersion
		if in.PasswordHash != stored.PasswordHash || in.Identity != stored.Identity || in.Disabled != stored.Disabled ||
			removeIdentities && len(stored.Identities) > 0 {
			c.TokenVersion++
		}
		c.Identities = stored.Identities
		if stored.Identity != in.Identity {
			c.Identities = withoutIdentity(c.Identities, stored.Identity)
		}
	} else {
		u.s.userOrder = append(u.s.userOrder, in.ID)
	}
	if removeIdentities {
		c.Identities = nil
	}
	if !in.Identity.IsZero() && !c.HasIdentity(in.Identity) {
		c.Identities = append(c.Identities, directory.UserIdentity{Identity: in.Identity, CreatedAt: now()})
	}
	if in.Identity.IsZero() && len(c.Identities) > 0 {
		c.Identity = c.Identities[0].Identity
	}
	touch(c)
	u.s.users[in.ID] = c
	return nil
}

// checkUnique refuses a username, email or external id another account than id holds.
func (u userStore) checkUnique(id, username, email string, ext directory.External) error {
	for otherID, x := range u.s.users {
		switch {
		case otherID == id:
		case x.Username == username:
			return fmt.Errorf("%w: username %q is taken", directory.ErrConflict, username)
		case email != "" && x.Email == email:
			return fmt.Errorf("%w: %q", directory.ErrEmailTaken, email)
		case ext.ID != "" && x.External == ext:
			return fmt.Errorf("%w: external id is taken", directory.ErrConflict)
		}
	}
	return nil
}

// touch records a write to the stored account, as the Postgres trigger does.
func touch(x *directory.User) {
	v, _ := strconv.ParseInt(x.ResourceVersion, 10, 64)
	x.ResourceVersion = directory.FormatResourceVersion(v + 1)
	x.UpdatedAt = now()
}

func (u userStore) SetDisabled(_ context.Context, id string, disabled bool) (bool, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	x, ok := u.s.users[id]
	if !ok || x.Disabled == disabled {
		return false, nil
	}
	x.Disabled = disabled
	x.TokenVersion++
	touch(x)
	return true, nil
}

func (u userStore) UpdateProvisioned(_ context.Context, source, id string, in directory.ProvisionedUser) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	x, ok := u.s.users[id]
	if !ok || x.External.Source == "" || x.External.Source != source {
		return fmt.Errorf("%w: user %s of source %q", directory.ErrNotFound, id, source)
	}
	if err := u.checkUnique(id, in.Username, in.Email, directory.External{Source: source, ID: in.ExternalID}); err != nil {
		return err
	}
	x.Username, x.Email, x.External.ID = in.Username, in.Email, in.ExternalID
	touch(x)
	return nil
}

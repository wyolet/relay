package memstore

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/wyolet/relay/auth/directory"
)

type userStore struct{ s *Store }

func (u userStore) Get(_ context.Context, id string) (*directory.User, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	return cloneUser(u.s.users[id]), nil
}

func (u userStore) ByUsername(_ context.Context, username string) (*directory.User, error) {
	return u.find(func(x *directory.User) bool { return x.Username == username }), nil
}

func (u userStore) ByIdentity(_ context.Context, id directory.Identity) (*directory.User, error) {
	if id.IsZero() {
		return nil, nil
	}
	return u.find(func(x *directory.User) bool { return x.HasIdentity(id) }), nil
}

func (u userStore) ByExternalID(_ context.Context, externalID string) ([]*directory.User, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	var out []*directory.User
	for _, id := range u.s.userOrder {
		if x := u.s.users[id]; x.External.Source != "" && x.External.ID == externalID {
			out = append(out, cloneUser(x))
		}
	}
	return out, nil
}

func (u userStore) find(match func(*directory.User) bool) *directory.User {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	for _, x := range u.s.users {
		if match(x) {
			return cloneUser(x)
		}
	}
	return nil
}

func (u userStore) List(_ context.Context) ([]*directory.User, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	out := make([]*directory.User, 0, len(u.s.userOrder))
	for _, id := range u.s.userOrder {
		out = append(out, cloneUser(u.s.users[id]))
	}
	return out, nil
}

func (u userStore) Upsert(_ context.Context, in *directory.User) error {
	want, conditional, err := in.ExpectedVersion()
	if err != nil {
		return err
	}
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
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
		if in.PasswordHash != stored.PasswordHash || in.Identity != stored.Identity || in.Disabled != stored.Disabled {
			c.TokenVersion++
		}
		c.Identities = stored.Identities
		if stored.Identity != in.Identity {
			c.Identities = withoutIdentity(c.Identities, stored.Identity)
		}
	} else {
		u.s.userOrder = append(u.s.userOrder, in.ID)
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

func (u userStore) Delete(_ context.Context, id string) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if _, ok := u.s.users[id]; !ok {
		return nil
	}
	delete(u.s.users, id)
	u.s.userOrder = slices.DeleteFunc(u.s.userOrder, func(x string) bool { return x == id })
	u.s.dropUser(id)
	return nil
}

func (u userStore) BumpTokenVersion(_ context.Context, id string) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if x, ok := u.s.users[id]; ok {
		x.TokenVersion++
		touch(x)
	}
	return nil
}

func (u userStore) AddIdentity(_ context.Context, userID string, id directory.UserIdentity) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	x, ok := u.s.users[userID]
	if !ok {
		return invalid("identity: user %q does not exist", userID)
	}
	for otherID, other := range u.s.users {
		if otherID != userID && other.HasIdentity(id.Identity) {
			return fmt.Errorf("%w: identity is taken", directory.ErrConflict)
		}
		if id.ExternalID != "" && slices.ContainsFunc(other.Identities, func(o directory.UserIdentity) bool {
			return o.Issuer == id.Issuer && o.ExternalID == id.ExternalID && (otherID != userID || o.Identity != id.Identity)
		}) {
			return fmt.Errorf("%w: external id is taken", directory.ErrConflict)
		}
	}
	x.TokenVersion++
	touch(x)
	if i := slices.IndexFunc(x.Identities, func(o directory.UserIdentity) bool { return o.Identity == id.Identity }); i >= 0 {
		x.Identities[i].ExternalID = id.ExternalID
		return nil
	}
	x.Identities = append(x.Identities, directory.UserIdentity{Identity: id.Identity, ExternalID: id.ExternalID, CreatedAt: now()})
	if x.Identity.IsZero() {
		x.Identity = id.Identity
	}
	return nil
}

func (u userStore) AddFirstIdentity(_ context.Context, userID string, id directory.Identity) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	x, ok := u.s.users[userID]
	if !ok {
		return invalid("identity: user %q does not exist", userID)
	}
	if !x.Identity.IsZero() || len(x.Identities) > 0 {
		return fmt.Errorf("%w: user already holds an identity", directory.ErrConflict)
	}
	for _, other := range u.s.users {
		if other.HasIdentity(id) {
			return fmt.Errorf("%w: identity is taken", directory.ErrConflict)
		}
	}
	x.Identities = []directory.UserIdentity{{Identity: id, CreatedAt: now()}}
	x.Identity = id
	x.TokenVersion++
	touch(x)
	return nil
}

func (u userStore) RemoveIdentity(_ context.Context, userID string, id directory.Identity) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	x, ok := u.s.users[userID]
	if !ok {
		return nil
	}
	x.Identities = withoutIdentity(x.Identities, id)
	if x.Identity == id {
		x.Identity = directory.Identity{}
		if len(x.Identities) > 0 {
			x.Identity = x.Identities[0].Identity
		}
	}
	x.TokenVersion++
	touch(x)
	return nil
}

func (u userStore) RemoveIdentities(_ context.Context, userID string) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if x, ok := u.s.users[userID]; ok {
		x.Identities, x.Identity = nil, directory.Identity{}
		x.TokenVersion++
		touch(x)
	}
	return nil
}

func withoutIdentity(ids []directory.UserIdentity, id directory.Identity) []directory.UserIdentity {
	out := slices.DeleteFunc(slices.Clone(ids), func(o directory.UserIdentity) bool { return o.Identity == id })
	if len(out) == 0 {
		return nil
	}
	return out
}

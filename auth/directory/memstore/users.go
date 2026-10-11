package memstore

import (
	"context"
	"fmt"
	"slices"

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
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	for id, x := range u.s.users {
		if id == in.ID {
			continue
		}
		switch {
		case x.Username == in.Username:
			return fmt.Errorf("%w: username %q is taken", directory.ErrConflict, in.Username)
		case in.Email != "" && x.Email == in.Email:
			return fmt.Errorf("%w: %q", directory.ErrEmailTaken, in.Email)
		case !in.Identity.IsZero() && x.HasIdentity(in.Identity):
			return fmt.Errorf("%w: identity is taken", directory.ErrConflict)
		case in.External.ID != "" && x.External == in.External:
			return fmt.Errorf("%w: external id is taken", directory.ErrConflict)
		}
	}
	c := cloneUser(in)
	stored := u.s.users[in.ID]
	c.TokenVersion, c.CreatedAt, c.Identities = 1, now(), nil
	if stored != nil {
		c.TokenVersion, c.CreatedAt = stored.TokenVersion, stored.CreatedAt
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
	c.UpdatedAt = now()
	u.s.users[in.ID] = c
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
		x.UpdatedAt = now()
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
	x.UpdatedAt = now()
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
	return nil
}

func (u userStore) RemoveIdentities(_ context.Context, userID string) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if x, ok := u.s.users[userID]; ok {
		x.Identities, x.Identity = nil, directory.Identity{}
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

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
	key := id.Canonical()
	for otherID, other := range u.s.users {
		if otherID != userID && other.HasIdentity(key) {
			return fmt.Errorf("%w: identity is taken", directory.ErrConflict)
		}
		if id.ExternalID != "" && slices.ContainsFunc(other.Identities, func(o directory.UserIdentity) bool {
			return o.Issuer == key.Issuer && o.ExternalID == id.ExternalID && (otherID != userID || o.Identity != key)
		}) {
			return fmt.Errorf("%w: external id is taken", directory.ErrConflict)
		}
	}
	x.TokenVersion++
	touch(x)
	if i := slices.IndexFunc(x.Identities, func(o directory.UserIdentity) bool { return o.Identity == key }); i >= 0 {
		x.Identities[i].ExternalID = id.ExternalID
		return nil
	}
	x.Identities = append(x.Identities, directory.UserIdentity{Identity: key, ExternalID: id.ExternalID, CreatedAt: now()})
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
	x.Identities = []directory.UserIdentity{{Identity: id.Canonical(), CreatedAt: now()}}
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
	if x.Identity.Canonical() == id.Canonical() {
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
	id = id.Canonical()
	out := slices.DeleteFunc(slices.Clone(ids), func(o directory.UserIdentity) bool { return o.Identity == id })
	if len(out) == 0 {
		return nil
	}
	return out
}

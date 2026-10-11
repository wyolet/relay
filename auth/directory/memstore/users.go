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
	return u.find(func(x *directory.User) bool { return x.Identity == id }), nil
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
			return fmt.Errorf("%w: email %q is taken", directory.ErrConflict, in.Email)
		case !in.Identity.IsZero() && x.Identity == in.Identity:
			return fmt.Errorf("%w: identity is taken", directory.ErrConflict)
		}
	}
	c := cloneUser(in)
	stored := u.s.users[in.ID]
	c.TokenVersion, c.CreatedAt = 1, now()
	if stored != nil {
		c.TokenVersion, c.CreatedAt = stored.TokenVersion, stored.CreatedAt
	} else {
		u.s.userOrder = append(u.s.userOrder, in.ID)
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

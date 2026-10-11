package manifest

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
)

// Write stores one translated row through dir: a create when the store has no row with its id, an update otherwise. It is the write Apply runs, for a product that plans with its own loader; the directory's validation and grant checks run either way. row is a *directory.Team, Project, ServiceAccount, Role, User, Group or RoleBinding.
func Write(ctx context.Context, dir *directory.Directory, caller *principal.Principal, row any) error {
	s := dir.Stores()
	switch x := row.(type) {
	case *directory.Team:
		return upsert(ctx, s.Teams, x.Meta.ID, func() error { return dir.CreateTeam(ctx, x) }, func() error { return dir.UpdateTeam(ctx, x) })
	case *directory.Project:
		return upsert(ctx, s.Projects, x.Meta.ID, func() error { return dir.CreateProject(ctx, x) }, func() error { return dir.UpdateProject(ctx, x) })
	case *directory.ServiceAccount:
		return upsert(ctx, s.ServiceAccounts, x.Meta.ID, func() error { return dir.CreateServiceAccount(ctx, x) }, func() error { return dir.UpdateServiceAccount(ctx, x) })
	case *directory.Role:
		return upsert(ctx, s.Roles, x.Meta.ID, func() error { return dir.CreateRole(ctx, x) }, func() error { return dir.UpdateRole(ctx, caller, x) })
	case *directory.Group:
		return upsert(ctx, s.Groups, x.Meta.ID, func() error { return dir.CreateGroup(ctx, x) }, func() error { return dir.UpdateGroup(ctx, x) })
	case *directory.RoleBinding:
		return upsert(ctx, s.RoleBindings, x.Meta.ID, func() error { return dir.CreateRoleBinding(ctx, caller, x) }, func() error { return dir.UpdateRoleBinding(ctx, caller, x) })
	case *directory.User:
		current, err := s.Users.Get(ctx, x.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return dir.CreateUser(ctx, caller, x)
		}
		return updateUser(ctx, dir, caller, current, x)
	}
	return fmt.Errorf("manifest: cannot write a %T", row)
}

// updateUser writes a translated account over current. A translation with no identity means the document cleared it, since an omitted one keeps current's: every stored identity goes in one directory call, which refuses it for an account without a password. Clearing only the primary would promote the next one, and a re-apply would change the account again.
func updateUser(ctx context.Context, dir *directory.Directory, caller *principal.Principal, current, u *directory.User) error {
	if !u.Identity.IsZero() || len(current.Identities) == 0 {
		return dir.UpdateUser(ctx, caller, u)
	}
	keep := *u
	keep.Identity = current.Identity
	if err := dir.UpdateUser(ctx, caller, &keep); err != nil {
		return err
	}
	return dir.RemoveIdentities(ctx, u.ID)
}

func upsert[T any](ctx context.Context, s directory.Store[T], id string, create, update func() error) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if current == nil {
		return create()
	}
	return update()
}

// Delete removes the row of kind with id through dir.
func Delete(ctx context.Context, dir *directory.Directory, kind, id string) error {
	switch kind {
	case KindTeam:
		return dir.DeleteTeam(ctx, id)
	case KindProject:
		return dir.DeleteProject(ctx, id)
	case KindServiceAccount:
		return dir.DeleteServiceAccount(ctx, id)
	case KindRole:
		return dir.DeleteRole(ctx, id)
	case KindUser:
		return dir.DeleteUser(ctx, id)
	case KindGroup:
		return dir.DeleteGroup(ctx, id)
	case KindRoleBinding:
		return dir.DeleteRoleBinding(ctx, id)
	}
	return fmt.Errorf("manifest: unknown kind %q", kind)
}

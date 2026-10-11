package control

import (
	"context"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
)

// kindWriter is a store whose create and update are distinct operations. The CRUD factory calls them instead of Upsert when the store has them.
type kindWriter[T any] interface {
	Create(ctx context.Context, t *T) error
	Update(ctx context.Context, t *T) error
}

type directoryOp[D any] func(dir *directory.Directory, ctx context.Context, p *principal.Principal, row *D) error

// ungranted adapts an operation that hands out no permission, so it takes no principal.
func ungranted[D any](op func(*directory.Directory, context.Context, *D) error) directoryOp[D] {
	return func(dir *directory.Directory, ctx context.Context, _ *principal.Principal, row *D) error {
		return op(dir, ctx, row)
	}
}

// directoryKind reads through the relay store and writes through the directory, converting each row on the way in.
type directoryKind[T, D any] struct {
	entityStore[T]
	ops    directoryOps
	toRow  func(*T) (*D, error)
	create directoryOp[D]
	update directoryOp[D]
	remove func(dir *directory.Directory, ctx context.Context, id string) error
}

func (k directoryKind[T, D]) Create(ctx context.Context, t *T) error {
	return k.write(ctx, t, k.create)
}

func (k directoryKind[T, D]) Update(ctx context.Context, t *T) error {
	return k.write(ctx, t, k.update)
}

func (k directoryKind[T, D]) Delete(ctx context.Context, id string) error {
	dir, err := k.ops.directory()
	if err != nil {
		return err
	}
	return k.remove(dir, ctx, id)
}

func (k directoryKind[T, D]) write(ctx context.Context, t *T, op directoryOp[D]) error {
	dir, err := k.ops.directory()
	if err != nil {
		return err
	}
	row, err := k.toRow(t)
	if err != nil {
		return err
	}
	return meta.FromDirectoryError(op(dir, ctx, principalOf(ctx, k.ops.authz), row))
}

func infallible[T, D any](to func(*T) *D) func(*T) (*D, error) {
	return func(t *T) (*D, error) { return to(t), nil }
}

// directoryKinds are the CRUD stores of the kinds the directory owns.
type directoryKinds struct {
	teams           directoryKind[team.Team, directory.Team]
	projects        directoryKind[project.Project, directory.Project]
	serviceAccounts directoryKind[serviceaccount.ServiceAccount, directory.ServiceAccount]
	groups          directoryKind[group.Group, directory.Group]
	roles           directoryKind[role.Role, directory.Role]
	roleBindings    directoryKind[rolebinding.RoleBinding, directory.RoleBinding]
}

func newDirectoryKinds(d Deps, ops directoryOps) directoryKinds {
	s := d.Stores
	type dir = directory.Directory
	return directoryKinds{
		teams: directoryKind[team.Team, directory.Team]{
			entityStore: s.Team, ops: ops, toRow: (*team.Team).ToDirectory,
			create: ungranted((*dir).CreateTeam), update: ungranted((*dir).UpdateTeam), remove: (*dir).DeleteTeam,
		},
		projects: directoryKind[project.Project, directory.Project]{
			entityStore: s.Project, ops: ops, toRow: (*project.Project).ToDirectory,
			create: ungranted((*dir).CreateProject), update: ungranted((*dir).UpdateProject), remove: (*dir).DeleteProject,
		},
		serviceAccounts: directoryKind[serviceaccount.ServiceAccount, directory.ServiceAccount]{
			entityStore: s.ServiceAccount, ops: ops, toRow: (*serviceaccount.ServiceAccount).ToDirectory,
			create: ungranted((*dir).CreateServiceAccount), update: ungranted((*dir).UpdateServiceAccount), remove: (*dir).DeleteServiceAccount,
		},
		groups: directoryKind[group.Group, directory.Group]{
			entityStore: s.Group, ops: ops, toRow: infallible((*group.Group).ToDirectory),
			create: (*dir).CreateGroup, update: (*dir).UpdateGroup, remove: (*dir).DeleteGroup,
		},
		roles: directoryKind[role.Role, directory.Role]{
			entityStore: s.Role, ops: ops, toRow: infallible((*role.Role).ToDirectory),
			create: ungranted((*dir).CreateRole), update: (*dir).UpdateRole, remove: (*dir).DeleteRole,
		},
		roleBindings: directoryKind[rolebinding.RoleBinding, directory.RoleBinding]{
			entityStore: s.RoleBinding, ops: ops, toRow: infallible((*rolebinding.RoleBinding).ToDirectory),
			create: (*dir).CreateRoleBinding, update: (*dir).UpdateRoleBinding, remove: (*dir).DeleteRoleBinding,
		},
	}
}

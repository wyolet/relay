package apply

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/auth/directory"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// The auth module's kinds (Team, Project, ServiceAccount, Group, Role, RoleBinding, User) are written through its directory, so the module's validation and grant checks hold apply's rows to the same rules as every other write path. Relay's own plan, authorization, governance and prune rules still run first.

var errNoDirectory = errors.New("apply: the account, tenancy and RBAC stores are not all wired")

// directoryFor is opts.Directory, or a directory over opts.Stores whose grant check probes opts.Authz.
func directoryFor(opts Options) (*directory.Directory, error) {
	if opts.Directory != nil {
		return opts.Directory, nil
	}
	s := opts.Stores
	if s.Team == nil || s.Project == nil || s.ServiceAccount == nil || s.Group == nil ||
		s.Role == nil || s.RoleBinding == nil || s.User == nil {
		return nil, errNoDirectory
	}
	return directory.New(directory.Config{
		Stores: directory.Stores{
			Users: s.User.Directory(), Groups: s.Group.Directory(), Teams: s.Team.Directory(),
			Projects: s.Project.Directory(), ServiceAccounts: s.ServiceAccount.Directory(),
			Roles: s.Role.Directory(), RoleBindings: s.RoleBinding.Directory(),
		},
		Grants: refcheck.Grants{Authz: opts.Authz},
		Kinds:  role.Kinds,
		Verbs:  role.Verbs,
	})
}

// throughDirectory is a kind's Upsert: the relay row converted to the directory's and written as the caller in ctx.
func throughDirectory[T any](b *builder, toRow func(*T) (any, error)) func(context.Context, *T) error {
	return func(ctx context.Context, row *T) error {
		if b.dirErr != nil {
			return b.dirErr
		}
		r, err := toRow(row)
		if err != nil {
			return err
		}
		return authmanifest.Write(ctx, b.dir, refcheck.Principal(ctx, b.opts.Authz), r)
	}
}

// deleteThroughDirectory is a kind's Delete.
func deleteThroughDirectory(b *builder, kind string) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if b.dirErr != nil {
			return b.dirErr
		}
		return authmanifest.Delete(ctx, b.dir, kind, id)
	}
}

// converted adapts a fallible ToDirectory to throughDirectory.
func converted[T, D any](to func(*T) (*D, error)) func(*T) (any, error) {
	return func(t *T) (any, error) {
		d, err := to(t)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
}

// infallible adapts a ToDirectory that cannot fail.
func infallible[T, D any](to func(*T) *D) func(*T) (any, error) {
	return func(t *T) (any, error) { return to(t), nil }
}

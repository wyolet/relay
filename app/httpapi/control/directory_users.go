package control

import (
	"context"

	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
)

// directoryUsers reads accounts through the relay store and writes them through the directory.
type directoryUsers struct {
	*user.Store
	ops directoryOps
}

// Create adds an account with no roles, as a first sign-in does, so it needs no caller.
func (u directoryUsers) Create(ctx context.Context, row *user.User) error {
	dir, err := u.ops.directory()
	if err != nil {
		return err
	}
	return dir.CreateUser(ctx, nil, row.ToDirectory())
}

func (u directoryUsers) UpdateUser(ctx context.Context, p *principal.Principal, row *directory.User) error {
	dir, err := u.ops.directory()
	if err != nil {
		return err
	}
	return dir.UpdateUser(ctx, p, row)
}

func (u directoryUsers) DisableUser(ctx context.Context, id string) error {
	dir, err := u.ops.directory()
	if err != nil {
		return err
	}
	return dir.DisableUser(ctx, id)
}

func (u directoryUsers) EnableUser(ctx context.Context, id string) error {
	dir, err := u.ops.directory()
	if err != nil {
		return err
	}
	return dir.EnableUser(ctx, id)
}

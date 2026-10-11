package control

import (
	"context"

	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/scim"
)

// directoryUsers reads accounts through the relay store and writes them through the directory.
type directoryUsers struct {
	*user.Store
	ops directoryOps
	// clients are the SCIM clients, for telling a revoked client's accounts apart; nil links no provisioned account.
	clients scim.TokenStore
}

// Create adds an account with no roles, as a first sign-in does, so it needs no caller.
func (u directoryUsers) Create(ctx context.Context, row *user.User) error {
	dir, err := u.ops.directory()
	if err != nil {
		return err
	}
	return dir.CreateUser(ctx, nil, row.ToDirectory())
}

// UnlinkedByExternalID returns the SCIM-provisioned accounts with externalID that no sign-in identity reached yet, from clients that are not revoked: a revoked client is no longer trusted to say whose an account is.
func (u directoryUsers) UnlinkedByExternalID(ctx context.Context, externalID string) ([]*user.User, error) {
	dir, err := u.ops.directory()
	if err != nil {
		return nil, err
	}
	rows, err := dir.UnlinkedUsersByExternalID(ctx, externalID)
	if err != nil || len(rows) == 0 || u.clients == nil {
		return nil, err
	}
	clients, err := u.clients.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(clients))
	for _, c := range clients {
		live[c.Name] = !c.Revoked
	}
	out := make([]*user.User, 0, len(rows))
	for _, r := range rows {
		if live[r.External.Source] {
			out = append(out, user.FromDirectory(r))
		}
	}
	return out, nil
}

func (u directoryUsers) AddFirstIdentity(ctx context.Context, userID string, id directory.Identity) error {
	dir, err := u.ops.directory()
	if err != nil {
		return err
	}
	return dir.AddFirstIdentity(ctx, userID, id)
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

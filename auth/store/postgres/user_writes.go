package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

func (s users) Upsert(ctx context.Context, u *directory.User) error {
	row, err := upsertParams(u)
	if err != nil {
		return err
	}
	if !u.Identity.IsZero() {
		return mapErr("upsert user", staleIfNoRows(gen.New(s.db).UpsertUser(ctx, row)))
	}
	// Clearing the primary of an account that holds other identities promotes the oldest of them.
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := mapErr("upsert user", staleIfNoRows(q.UpsertUser(ctx, row))); err != nil {
			return err
		}
		return mapErr("promote identity", q.PromoteOldestIdentity(ctx, gen.PromoteOldestIdentityParams{ID: u.ID}))
	})
}

func (s users) UpsertRemovingIdentities(ctx context.Context, u *directory.User) error {
	if !u.Identity.IsZero() {
		return fmt.Errorf("%w: user %q: removing every identity needs a zero primary", directory.ErrInvalid, u.Username)
	}
	row, err := upsertParams(u)
	if err != nil {
		return err
	}
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := mapErr("upsert user", staleIfNoRows(q.UpsertUser(ctx, row))); err != nil {
			return err
		}
		if err := q.DeleteUserIdentities(ctx, u.ID); err != nil {
			return mapErr("remove identities", err)
		}
		return mapErr("bump token version", q.BumpUserTokenVersion(ctx, u.ID))
	})
}

func upsertParams(u *directory.User) (gen.UpsertUserParams, error) {
	roles := u.Roles
	if roles == nil {
		roles = []string{}
	}
	v, conditional, err := u.ExpectedVersion()
	if err != nil {
		return gen.UpsertUserParams{}, err
	}
	return gen.UpsertUserParams{
		ID:              u.ID,
		Username:        u.Username,
		Email:           text(u.Email),
		PasswordHash:    text(u.PasswordHash),
		OidcSubject:     text(encodeIdentity(u.Identity)),
		Roles:           roles,
		Disabled:        u.Disabled,
		ExternalSource:  text(u.External.Source),
		ExternalID:      text(u.External.ID),
		ExpectedVersion: pgtype.Int8{Int64: v, Valid: conditional},
	}, nil
}

func (s users) SetDisabled(ctx context.Context, id string, disabled bool) (bool, error) {
	n, err := gen.New(s.db).SetUserDisabled(ctx, gen.SetUserDisabledParams{ID: id, Disabled: disabled})
	if err != nil {
		return false, mapErr("set disabled", err)
	}
	return n > 0, nil
}

func (s users) UpdateProvisioned(ctx context.Context, source, id string, in directory.ProvisionedUser) error {
	n, err := gen.New(s.db).UpdateProvisionedUser(ctx, gen.UpdateProvisionedUserParams{
		ID: id, ExternalSource: text(source), Username: in.Username, Email: text(in.Email), ExternalID: text(in.ExternalID),
	})
	if err != nil {
		return mapErr("update provisioned user", err)
	}
	if n == 0 {
		return fmt.Errorf("update provisioned user: %w: user %s of source %q", directory.ErrNotFound, id, source)
	}
	return nil
}

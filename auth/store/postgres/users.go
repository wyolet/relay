package postgres

import (
	"context"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

type users struct{ db DB }

func (s users) Get(ctx context.Context, id string) (*directory.User, error) {
	return userOrNil(gen.New(s.db).GetUser(ctx, id))
}

func (s users) ByUsername(ctx context.Context, username string) (*directory.User, error) {
	return userOrNil(gen.New(s.db).GetUserByUsername(ctx, username))
}

func (s users) ByIdentity(ctx context.Context, id directory.Identity) (*directory.User, error) {
	if id.IsZero() {
		return nil, nil
	}
	return userOrNil(gen.New(s.db).GetUserByOIDCSubject(ctx, text(encodeIdentity(id))))
}

func (s users) List(ctx context.Context) ([]*directory.User, error) {
	rows, err := gen.New(s.db).ListUsers(ctx)
	if err != nil {
		return nil, mapErr("list users", err)
	}
	out := make([]*directory.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userFromRow(r))
	}
	return out, nil
}

func (s users) Upsert(ctx context.Context, u *directory.User) error {
	roles := u.Roles
	if roles == nil {
		roles = []string{}
	}
	return mapErr("upsert user", gen.New(s.db).UpsertUser(ctx, gen.UpsertUserParams{
		ID:           u.ID,
		Username:     u.Username,
		Email:        text(u.Email),
		PasswordHash: text(u.PasswordHash),
		OidcSubject:  text(encodeIdentity(u.Identity)),
		Roles:        roles,
		Disabled:     u.Disabled,
	}))
}

func (s users) Delete(ctx context.Context, id string) error {
	return mapErr("delete user", gen.New(s.db).DeleteUser(ctx, id))
}

func (s users) BumpTokenVersion(ctx context.Context, id string) error {
	return mapErr("bump token version", gen.New(s.db).BumpUserTokenVersion(ctx, id))
}

func userOrNil(r gen.User, err error) (*directory.User, error) {
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get user", err)
	}
	return userFromRow(r), nil
}

func userFromRow(r gen.User) *directory.User {
	return &directory.User{
		ID:           r.ID,
		Username:     r.Username,
		Email:        r.Email.String,
		PasswordHash: r.PasswordHash.String,
		Identity:     decodeIdentity(r.OidcSubject.String),
		Roles:        r.Roles,
		Disabled:     r.Disabled,
		TokenVersion: int(r.TokenVersion),
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

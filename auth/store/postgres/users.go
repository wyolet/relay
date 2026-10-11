// The primary identity is written to users.oidc_subject, and a trigger mirrors that column into identities (see the 000002 migration), so Upsert writes no identity row itself and a binary that only knows the column stays in step.

package postgres

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

type users struct{ db DB }

func (s users) Get(ctx context.Context, id string) (*directory.User, error) {
	q := gen.New(s.db)
	r, err := q.GetUser(ctx, id)
	return withIdentities(ctx, q, r, err)
}

func (s users) ByUsername(ctx context.Context, username string) (*directory.User, error) {
	q := gen.New(s.db)
	r, err := q.GetUserByUsername(ctx, username)
	return withIdentities(ctx, q, r, err)
}

func (s users) ByIdentity(ctx context.Context, id directory.Identity) (*directory.User, error) {
	if id.IsZero() {
		return nil, nil
	}
	q := gen.New(s.db)
	r, err := q.GetUserByIdentity(ctx, gen.GetUserByIdentityParams{Issuer: id.Issuer, Subject: id.Subject})
	return withIdentities(ctx, q, r, err)
}

func (s users) List(ctx context.Context) ([]*directory.User, error) {
	q := gen.New(s.db)
	rows, err := q.ListUsers(ctx)
	if err != nil {
		return nil, mapErr("list users", err)
	}
	ids, err := q.ListIdentities(ctx)
	if err != nil {
		return nil, mapErr("list identities", err)
	}
	byUser := map[string][]directory.UserIdentity{}
	for _, r := range ids {
		byUser[r.UserID] = append(byUser[r.UserID], identityFromRow(r))
	}
	out := make([]*directory.User, 0, len(rows))
	for _, r := range rows {
		u := userFromRow(r)
		u.Identities = byUser[u.ID]
		out = append(out, u)
	}
	return out, nil
}

func (s users) Upsert(ctx context.Context, u *directory.User) error {
	roles := u.Roles
	if roles == nil {
		roles = []string{}
	}
	row := gen.UpsertUserParams{
		ID:             u.ID,
		Username:       u.Username,
		Email:          text(u.Email),
		PasswordHash:   text(u.PasswordHash),
		OidcSubject:    text(encodeIdentity(u.Identity)),
		Roles:          roles,
		Disabled:       u.Disabled,
		ExternalSource: text(u.External.Source),
		ExternalID:     text(u.External.ID),
	}
	if !u.Identity.IsZero() {
		return mapErr("upsert user", gen.New(s.db).UpsertUser(ctx, row))
	}
	// Clearing the primary of an account that holds other identities promotes the oldest of them.
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := q.UpsertUser(ctx, row); err != nil {
			return mapErr("upsert user", err)
		}
		return mapErr("promote identity", q.PromoteOldestIdentity(ctx, gen.PromoteOldestIdentityParams{ID: u.ID}))
	})
}

func (s users) Delete(ctx context.Context, id string) error {
	return mapErr("delete user", gen.New(s.db).DeleteUser(ctx, id))
}

func (s users) BumpTokenVersion(ctx context.Context, id string) error {
	return mapErr("bump token version", gen.New(s.db).BumpUserTokenVersion(ctx, id))
}

func (s users) AddIdentity(ctx context.Context, userID string, id directory.UserIdentity) error {
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		n, err := q.UpsertIdentity(ctx, gen.UpsertIdentityParams{
			UserID: userID, Issuer: id.Issuer, Subject: id.Subject, ExternalID: text(id.ExternalID),
		})
		if err != nil {
			return mapErr("add identity", err)
		}
		if n == 0 {
			return fmt.Errorf("add identity: %w: identity belongs to another user", directory.ErrConflict)
		}
		return mapErr("set primary identity", q.SetPrimaryIdentityIfNone(ctx, gen.SetPrimaryIdentityIfNoneParams{
			ID: userID, OidcSubject: text(encodeIdentity(id.Identity)),
		}))
	})
}

func (s users) RemoveIdentity(ctx context.Context, userID string, id directory.Identity) error {
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := q.DeleteIdentity(ctx, gen.DeleteIdentityParams{UserID: userID, Issuer: id.Issuer, Subject: id.Subject}); err != nil {
			return mapErr("remove identity", err)
		}
		return mapErr("promote identity", q.PromoteOldestIdentity(ctx, gen.PromoteOldestIdentityParams{
			ID: userID, OidcSubject: text(encodeIdentity(id)),
		}))
	})
}

// withIdentities turns a lookup's row into the user with its identities; no row is (nil, nil).
func withIdentities(ctx context.Context, q *gen.Queries, r gen.User, err error) (*directory.User, error) {
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get user", err)
	}
	u := userFromRow(r)
	rows, err := q.ListUserIdentities(ctx, u.ID)
	if err != nil {
		return nil, mapErr("list identities", err)
	}
	for _, r := range rows {
		u.Identities = append(u.Identities, identityFromRow(r))
	}
	return u, nil
}

func userFromRow(r gen.User) *directory.User {
	return &directory.User{
		ID:           r.ID,
		Username:     r.Username,
		Email:        r.Email.String,
		PasswordHash: r.PasswordHash.String,
		Identity:     decodeIdentity(r.OidcSubject.String),
		External:     directory.External{Source: r.ExternalSource.String, ID: r.ExternalID.String},
		Roles:        r.Roles,
		Disabled:     r.Disabled,
		TokenVersion: int(r.TokenVersion),
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

func identityFromRow(r gen.Identity) directory.UserIdentity {
	return directory.UserIdentity{
		Identity:   directory.Identity{Issuer: r.Issuer, Subject: r.Subject},
		ExternalID: r.ExternalID.String,
		CreatedAt:  r.CreatedAt.Time,
	}
}

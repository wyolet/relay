// The primary identity is written to users.oidc_subject as given, and a trigger mirrors that column into identities in canonical form (see the 000002 and 000005 migrations), so Upsert writes no identity row itself and a binary that only knows the column, matching it exactly, stays in step.

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
	id = id.Canonical()
	q := gen.New(s.db)
	r, err := q.GetUserByIdentity(ctx, gen.GetUserByIdentityParams{Issuer: id.Issuer, Subject: id.Subject})
	return withIdentities(ctx, q, r, err)
}

func (s users) ByExternalID(ctx context.Context, externalID string) ([]*directory.User, error) {
	if externalID == "" {
		return nil, nil
	}
	q := gen.New(s.db)
	rows, err := q.ListUsersByExternalID(ctx, text(externalID))
	if err != nil {
		return nil, mapErr("users by external id", err)
	}
	out := make([]*directory.User, 0, len(rows))
	for _, r := range rows {
		u, err := withIdentities(ctx, q, r, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
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

func (s users) Delete(ctx context.Context, id string) error {
	return mapErr("delete user", gen.New(s.db).DeleteUser(ctx, id))
}

func (s users) BumpTokenVersion(ctx context.Context, id string) error {
	return mapErr("bump token version", gen.New(s.db).BumpUserTokenVersion(ctx, id))
}

func (s users) AddIdentity(ctx context.Context, userID string, id directory.UserIdentity) error {
	key := id.Canonical()
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		n, err := q.UpsertIdentity(ctx, gen.UpsertIdentityParams{
			UserID: userID, Issuer: key.Issuer, Subject: key.Subject, ExternalID: text(id.ExternalID),
		})
		if err != nil {
			return mapErr("add identity", err)
		}
		if n == 0 {
			return fmt.Errorf("add identity: %w: identity belongs to another user", directory.ErrConflict)
		}
		if err := q.SetPrimaryIdentityIfNone(ctx, gen.SetPrimaryIdentityIfNoneParams{
			ID: userID, OidcSubject: text(encodeIdentity(id.Identity)),
		}); err != nil {
			return mapErr("set primary identity", err)
		}
		return mapErr("bump token version", q.BumpUserTokenVersion(ctx, userID))
	})
}

func (s users) AddFirstIdentity(ctx context.Context, userID string, id directory.Identity) error {
	n, err := gen.New(s.db).SetFirstPrimaryIdentity(ctx, gen.SetFirstPrimaryIdentityParams{ID: userID, OidcSubject: text(encodeIdentity(id))})
	if err != nil {
		return mapErr("add first identity", err)
	}
	if n == 0 {
		return fmt.Errorf("add first identity: %w: user is missing or already holds an identity", directory.ErrConflict)
	}
	return nil
}

func (s users) RemoveIdentity(ctx context.Context, userID string, id directory.Identity) error {
	key := id.Canonical()
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := q.DeleteIdentity(ctx, gen.DeleteIdentityParams{UserID: userID, Issuer: key.Issuer, Subject: key.Subject}); err != nil {
			return mapErr("remove identity", err)
		}
		// The promotion compares the primary as stored, which may spell the issuer with a trailing slash where id does not, or the reverse.
		named := text(encodeIdentity(id))
		r, err := q.GetUser(ctx, userID)
		if err != nil && !notFound(err) {
			return mapErr("get user", err)
		}
		if err == nil && decodeIdentity(r.OidcSubject.String).Canonical() == key {
			named = r.OidcSubject
		}
		if err := q.PromoteOldestIdentity(ctx, gen.PromoteOldestIdentityParams{
			ID: userID, OidcSubject: named,
		}); err != nil {
			return mapErr("promote identity", err)
		}
		return mapErr("bump token version", q.BumpUserTokenVersion(ctx, userID))
	})
}

func (s users) RemoveIdentities(ctx context.Context, userID string) error {
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := q.ClearPrimaryIdentity(ctx, userID); err != nil {
			return mapErr("clear primary identity", err)
		}
		if err := q.DeleteUserIdentities(ctx, userID); err != nil {
			return mapErr("remove identities", err)
		}
		return mapErr("bump token version", q.BumpUserTokenVersion(ctx, userID))
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
		ID:              r.ID,
		Username:        r.Username,
		Email:           r.Email.String,
		PasswordHash:    r.PasswordHash.String,
		Identity:        decodeIdentity(r.OidcSubject.String),
		External:        directory.External{Source: r.ExternalSource.String, ID: r.ExternalID.String},
		Roles:           r.Roles,
		Disabled:        r.Disabled,
		TokenVersion:    int(r.TokenVersion),
		ResourceVersion: directory.FormatResourceVersion(r.ResourceVersion),
		CreatedAt:       r.CreatedAt.Time,
		UpdatedAt:       r.UpdatedAt.Time,
	}
}

func identityFromRow(r gen.Identity) directory.UserIdentity {
	return directory.UserIdentity{
		Identity:   directory.Identity{Issuer: r.Issuer, Subject: r.Subject},
		ExternalID: r.ExternalID.String,
		CreatedAt:  r.CreatedAt.Time,
	}
}

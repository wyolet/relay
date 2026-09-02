// store.go is the data-access layer for User. Same conventions as the
// entity stores: sqlc-generated typed methods, no sqlc types in exported
// signatures, (nil, nil) on not-found.
package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the User data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// Get returns the user with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*User, error) {
	r, err := s.q.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("user.Get: %w", err)
	}
	return fromRow(r), nil
}

// ByUsername returns the user with the given username, or (nil, nil).
func (s *Store) ByUsername(ctx context.Context, username string) (*User, error) {
	r, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("user.ByUsername: %w", err)
	}
	return fromRow(r), nil
}

// ByOIDCSubject returns the user bound to the external identity subject
// ("issuer|sub"), or (nil, nil).
func (s *Store) ByOIDCSubject(ctx context.Context, subject string) (*User, error) {
	r, err := s.q.GetUserByOIDCSubject(ctx, text(subject))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("user.ByOIDCSubject: %w", err)
	}
	return fromRow(r), nil
}

// List returns every user, oldest first.
func (s *Store) List(ctx context.Context) ([]*User, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("user.List: %w", err)
	}
	out := make([]*User, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// Upsert writes u. Caller is responsible for stamping ID.
func (s *Store) Upsert(ctx context.Context, u *User) error {
	roles := u.Roles
	if roles == nil {
		roles = []string{}
	}
	err := s.q.UpsertUser(ctx, gen.UpsertUserParams{
		ID:           u.ID,
		Username:     u.Username,
		Email:        text(u.Email),
		PasswordHash: text(u.PasswordHash),
		OidcSubject:  text(u.OIDCSubject),
		Roles:        roles,
		Disabled:     u.Disabled,
	})
	if err != nil {
		return fmt.Errorf("user.Upsert: %w", err)
	}
	return nil
}

// Delete removes a user by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteUser(ctx, id)
}

// TokenVersions returns every user's current token version, keyed by id.
// The map is the snapshot's whole view of users — small enough to rebuild
// on every NOTIFY.
func (s *Store) TokenVersions(ctx context.Context) (map[string]int, error) {
	rows, err := s.q.ListUserTokenVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("user.TokenVersions: %w", err)
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.ID] = int(r.TokenVersion)
	}
	return out, nil
}

// BumpTokenVersion invalidates every token the user holds.
func (s *Store) BumpTokenVersion(ctx context.Context, id string) error {
	if err := s.q.BumpUserTokenVersion(ctx, id); err != nil {
		return fmt.Errorf("user.BumpTokenVersion: %w", err)
	}
	return nil
}

func fromRow(r gen.User) *User {
	return &User{
		ID:           r.ID,
		Username:     r.Username,
		Email:        r.Email.String,
		PasswordHash: r.PasswordHash.String,
		OIDCSubject:  r.OidcSubject.String,
		Roles:        r.Roles,
		Disabled:     r.Disabled,
		TokenVersion: int(r.TokenVersion),
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

// text maps "" ↔ SQL NULL so the partial unique indexes (email,
// oidc_subject) never collide on empty strings.
func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

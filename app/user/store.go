// store.go is the data-access layer for User. Accounts are stored by the auth module's directory store; the reads it has no equivalent for (batched existence and username lookups, and the snapshot's token versions and disabled ids) stay on relay's sqlc queries. (nil, nil) on not-found.

package user

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the User data-access type.
type Store struct {
	rows directory.UserStore
	q    *gen.Queries
}

// NewStore constructs a Store over db: the pool, or a transaction.
func NewStore(db storage.DB) *Store {
	return &Store{rows: authpg.New(db).Users, q: gen.New(db)}
}

// Directory is the auth module's store under this one, for writes that run through the directory.
func (s *Store) Directory() directory.UserStore { return s.rows }

// Get returns the user with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*User, error) {
	u, err := s.rows.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("user.Get: %w", err)
	}
	return FromDirectory(u), nil
}

// ByUsername returns the user with the given username, or (nil, nil).
func (s *Store) ByUsername(ctx context.Context, username string) (*User, error) {
	u, err := s.rows.ByUsername(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("user.ByUsername: %w", err)
	}
	return FromDirectory(u), nil
}

// ByOIDCSubject returns the user holding the external identity subject
// ("issuer|sub") among its sign-in identities, or (nil, nil).
func (s *Store) ByOIDCSubject(ctx context.Context, subject string) (*User, error) {
	u, err := s.rows.ByIdentity(ctx, oidcIdentity(subject))
	if err != nil {
		return nil, fmt.Errorf("user.ByOIDCSubject: %w", err)
	}
	return FromDirectory(u), nil
}

// List returns every user, oldest first.
func (s *Store) List(ctx context.Context) ([]*User, error) {
	rows, err := s.rows.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("user.List: %w", err)
	}
	out := make([]*User, 0, len(rows))
	for _, u := range rows {
		out = append(out, FromDirectory(u))
	}
	return out, nil
}

// MissingIDs returns the subset of ids with no users row, in one query.
// Callers validating a membership list would otherwise issue a Get per
// member.
func (s *Store) MissingIDs(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := s.q.ListUserIDsIn(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("user.MissingIDs: %w", err)
	}
	have := make(map[string]struct{}, len(found))
	for _, id := range found {
		have[id] = struct{}{}
	}
	var missing []string
	for _, id := range ids {
		if _, ok := have[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// Usernames maps each of ids that has a users row to its username, in one query. Ids without a row are absent.
func (s *Store) Usernames(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListUsernamesIn(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("user.Usernames: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Username
	}
	return out, nil
}

// Upsert writes u. Caller is responsible for stamping ID.
func (s *Store) Upsert(ctx context.Context, u *User) error {
	if err := s.rows.Upsert(ctx, u.ToDirectory()); err != nil {
		return fmt.Errorf("user.Upsert: %w", err)
	}
	return nil
}

// Delete removes a user by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rows.Delete(ctx, id)
}

// TokenVersionsAndDisabled returns every enabled user's current token version and the set of disabled user ids from one read. They are the snapshot's whole view of users — small enough to rebuild on every NOTIFY. A disabled account is absent from the versions, which is what makes its live tokens stop verifying.
func (s *Store) TokenVersionsAndDisabled(ctx context.Context) (map[string]int, map[string]struct{}, error) {
	rows, err := s.q.ListUserTokenVersionsAndDisabled(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("user.TokenVersionsAndDisabled: %w", err)
	}
	versions := make(map[string]int, len(rows))
	disabled := map[string]struct{}{}
	for _, r := range rows {
		if r.Disabled {
			disabled[r.ID] = struct{}{}
			continue
		}
		versions[r.ID] = int(r.TokenVersion)
	}
	return versions, disabled, nil
}

// BumpTokenVersion invalidates every token the user holds.
func (s *Store) BumpTokenVersion(ctx context.Context, id string) error {
	if err := s.rows.BumpTokenVersion(ctx, id); err != nil {
		return fmt.Errorf("user.BumpTokenVersion: %w", err)
	}
	return nil
}

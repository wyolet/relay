package directory

import "context"

// Store is the storage of one row kind. Get returns (nil, nil) when no row has the id. Upsert inserts or replaces by Meta.ID; with Meta.ResourceVersion set it replaces only while the stored row still carries that version and returns ErrStaleResourceVersion otherwise, and a duplicate name is ErrConflict. Delete of a missing id is not an error.
type Store[T any] interface {
	Get(ctx context.Context, id string) (*T, error)
	// List returns every row ordered by name.
	List(ctx context.Context) ([]*T, error)
	Upsert(ctx context.Context, row *T) error
	Delete(ctx context.Context, id string) error
}

// UserStore is the storage of accounts. Lookups return (nil, nil) when no account matches; a duplicate username, email, identity or external id is ErrConflict.
type UserStore interface {
	Get(ctx context.Context, id string) (*User, error)
	ByUsername(ctx context.Context, username string) (*User, error)
	// ByIdentity finds the account holding id among its identities, primary or not.
	ByIdentity(ctx context.Context, id Identity) (*User, error)
	// List returns every account, oldest first.
	List(ctx context.Context) ([]*User, error)
	// Upsert inserts or replaces by ID and leaves TokenVersion as stored; a new account starts at version 1. It writes Identity as the primary identity, replacing the stored primary when it differs, and ignores Identities; a zero Identity on an account that holds other identities makes the oldest of them primary, so such an account always has one.
	Upsert(ctx context.Context, u *User) error
	Delete(ctx context.Context, id string) error
	// BumpTokenVersion invalidates every token the user holds.
	BumpTokenVersion(ctx context.Context, id string) error
	// AddIdentity links id to the user, or updates its external id when the user already holds it. The first identity of an account with no primary becomes its primary.
	AddIdentity(ctx context.Context, userID string, id UserIdentity) error
	// RemoveIdentity unlinks id from the user; one the user does not hold is not an error. Removing the primary makes the oldest remaining identity primary.
	RemoveIdentity(ctx context.Context, userID string, id Identity) error
}

// Stores is every store the directory reads and writes. A delete cascades in the store: a team takes its projects, a project its service accounts, a role its bindings, and a user or service account leaves every group and binding it was in.
type Stores struct {
	Users           UserStore
	Groups          Store[Group]
	Teams           Store[Team]
	Projects        Store[Project]
	ServiceAccounts Store[ServiceAccount]
	Roles           Store[Role]
	RoleBindings    Store[RoleBinding]
}

func (s Stores) complete() bool {
	return s.Users != nil && s.Groups != nil && s.Teams != nil && s.Projects != nil &&
		s.ServiceAccounts != nil && s.Roles != nil && s.RoleBindings != nil
}

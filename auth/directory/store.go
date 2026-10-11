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

// UserStore is the storage of accounts. Lookups return (nil, nil) when no account matches; a duplicate username, email or identity is ErrConflict.
type UserStore interface {
	Get(ctx context.Context, id string) (*User, error)
	ByUsername(ctx context.Context, username string) (*User, error)
	ByIdentity(ctx context.Context, id Identity) (*User, error)
	// List returns every account, oldest first.
	List(ctx context.Context) ([]*User, error)
	// Upsert inserts or replaces by ID and leaves TokenVersion as stored; a new account starts at version 1.
	Upsert(ctx context.Context, u *User) error
	Delete(ctx context.Context, id string) error
	// BumpTokenVersion invalidates every token the user holds.
	BumpTokenVersion(ctx context.Context, id string) error
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

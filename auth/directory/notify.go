package directory

import "context"

// Kind names a row kind in a change notification.
type Kind string

const (
	KindUser           Kind = "user"
	KindGroup          Kind = "group"
	KindTeam           Kind = "team"
	KindProject        Kind = "project"
	KindServiceAccount Kind = "serviceaccount"
	KindRole           Kind = "role"
	KindRoleBinding    Kind = "rolebinding"
)

// Op is what happened to the row.
type Op string

const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
	// OpDisable is a user being disabled; re-enabling is an OpUpsert.
	OpDisable Op = "disable"
)

// Change is one committed write. A group membership change is an OpUpsert of the group. A delete is reported for the row named, not for the rows its store cascaded to.
type Change struct {
	Kind Kind
	Op   Op
	ID   string
}

// Notifier hears every committed write, so a product can drop state derived from the row. It runs synchronously after the write and must not block.
type Notifier interface {
	Notify(ctx context.Context, c Change)
}

// Revoker ends every session a user holds; *session.Manager satisfies it.
type Revoker interface {
	DestroyUser(ctx context.Context, userID string) error
}

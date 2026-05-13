// Package actor carries the identity of the caller making a request.
//
// An Actor is set by the auth middleware after a session lookup (or admin
// token bypass) and read by handlers via From(ctx). Today the only relevant
// fields are UserID + Username; ActiveOrgID/Roles are reserved for the
// multi-tenant + RBAC work that will land after the catalog cutover.
//
// Handlers should never read cookies or tokens directly — they go through
// the Actor in context. This keeps call sites stable across changes to the
// identity backend (YAML today, Postgres/Kratos/SuperTokens later).
package actor

import "context"

// Actor is the authenticated caller for the current request.
type Actor struct {
	// UserID is the durable reference (UUIDv7). Empty means "no user" —
	// usually the admin-token bypass path or anonymous inference traffic.
	UserID string

	// Username is the display name. Subject to change; never use it as
	// a join key.
	Username string

	// SessionID is the opaque session token (when this actor came from a
	// session lookup). Carried for audit + revocation. Empty for the
	// admin-token bypass.
	SessionID string

	// AdminToken is true when this actor came from the RELAY_ADMIN_TOKEN
	// bypass instead of a user session. Authz can grant blanket
	// permissions to break-glass callers.
	AdminToken bool

	// Future fields (intentionally empty in v1; populated by the multi-
	// tenant work):
	//   ActiveOrgID string
	//   Roles       []string
}

// IsAuthenticated reports whether the actor represents any kind of valid
// caller (real user session or admin-token bypass).
func (a *Actor) IsAuthenticated() bool {
	return a != nil && (a.UserID != "" || a.AdminToken)
}

type ctxKey struct{}

// WithActor returns a context carrying a.
func WithActor(ctx context.Context, a *Actor) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// From returns the actor in ctx, or nil if no actor is set.
func From(ctx context.Context) *Actor {
	a, _ := ctx.Value(ctxKey{}).(*Actor)
	return a
}

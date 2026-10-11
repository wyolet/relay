// Package actor carries the identity of the caller making a request.
//
// An Actor is relay's view of the auth module's principal: the control-plane middleware converts the principal its authenticator chain produced (FromPrincipal) and handlers read it via From(ctx).
//
// Handlers should never read cookies or tokens directly — they go through
// the Actor in context. This keeps call sites stable across changes to the
// identity backend (YAML today, Postgres/Kratos/SuperTokens later).
package actor

import (
	"context"

	"github.com/wyolet/relay/auth/principal"
)

// Actor is the authenticated caller for the current request.
type Actor struct {
	// UserID is the durable reference (UUIDv7). Empty means "no user" —
	// usually the admin-token bypass path or anonymous inference traffic.
	UserID string

	// Username is the display name. Subject to change; never use it as
	// a join key.
	Username string

	// SessionID identifies the session this actor came from, for audit
	// correlation. It is derived from, never equal to, the session token: it
	// is stored and shown to audit readers. Empty for the admin-token bypass.
	SessionID string

	// AdminToken is true when this actor came from the RELAY_ADMIN_TOKEN
	// bypass instead of a user session. Authz can grant blanket
	// permissions to break-glass callers.
	AdminToken bool

	// Roles are carried verbatim from the user record at session build;
	// app/authz interprets them. Empty for the admin-token bypass (which
	// AdminToken already marks as all-powerful).
	Roles []string

	// Subjects are the RBAC subject strings this caller acts under
	// ("user:<id>", "group:<name>", …). Built once per request by the
	// session middleware; handlers never assemble them. Empty for the
	// admin-token bypass, which authz short-circuits.
	Subjects []string

	// IdPGroups are the group names an identity provider asserted at login,
	// carried verbatim from the session. Empty for password logins and the
	// admin-token bypass.
	IdPGroups []string

	// Future fields (intentionally empty in v1; populated by the multi-
	// tenant work):
	//   ActiveOrgID string
}

// HasRole reports whether the actor carries role.
func (a *Actor) HasRole(role string) bool {
	if a == nil {
		return false
	}
	for _, r := range a.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsAuthenticated reports whether the actor represents any kind of valid
// caller (real user session or admin-token bypass).
func (a *Actor) IsAuthenticated() bool {
	return a != nil && (a.UserID != "" || a.AdminToken)
}

// FromPrincipal converts an authenticated principal to an Actor. The admin-kind principal is the admin-token bypass.
func FromPrincipal(p *principal.Principal) *Actor {
	return &Actor{
		UserID:     p.ID,
		Username:   p.Username,
		SessionID:  p.SessionID,
		AdminToken: p.Kind == principal.KindAdmin,
		Roles:      p.Roles,
		Subjects:   p.Subjects,
		IdPGroups:  p.Groups,
	}
}

// Principal converts a back to the module's principal, as the authorization engine reads it. Admin is set for the admin-token bypass only; which roles also make an admin is authz's rule. An Actor records no credential, so that part is left empty.
func (a *Actor) Principal() principal.Principal {
	p := principal.Principal{
		ID:        a.UserID,
		Kind:      principal.KindUser,
		Username:  a.Username,
		Groups:    a.IdPGroups,
		Subjects:  a.Subjects,
		Roles:     a.Roles,
		SessionID: a.SessionID,
	}
	if a.AdminToken {
		p.Kind, p.Admin = principal.KindAdmin, true
	}
	return p
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

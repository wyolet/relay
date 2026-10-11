// Package principal is the caller a request authenticated as: who it is, which credential proved it, and what that credential is limited to. auth/authn produces one and auth/rbac reads one; the accounts it names, and how they are stored, are the product's.
//
// It is a leaf: it imports nothing from the rest of the module, so every other package can depend on it.
package principal

import "context"

// Kind is what sort of caller a principal is.
type Kind string

const (
	KindUser           Kind = "user"
	KindServiceAccount Kind = "serviceaccount"
	// KindAdmin is a break-glass caller that stands for no account.
	KindAdmin Kind = "admin"
)

// Status is the state of the account a principal names. Empty means the product has not resolved it.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
	StatusDeleted  Status = "deleted"
)

// CredentialKind is how the caller proved who it is.
type CredentialKind string

const (
	CredentialSession CredentialKind = "session"
	CredentialAPIKey  CredentialKind = "api-key"
	CredentialToken   CredentialKind = "token"
	// CredentialService is a shared secret configured for a service, such as an operator's bearer.
	CredentialService CredentialKind = "service"
)

// Credential is what the request presented.
type Credential struct {
	Kind CredentialKind
	// KeyID and KeyName name the key or token presented, for audit. Empty for a session.
	KeyID   string
	KeyName string
	// Scope narrows what this credential may do below what the principal holds; nil leaves it unrestricted.
	Scope *CredentialScope
}

// Principal is an authenticated caller.
type Principal struct {
	// ID is the product's durable id for the account. Empty, with Admin unset, means unauthenticated.
	ID   string
	Kind Kind
	// Issuer and Subject are the stable identity an external identity provider asserted. Empty for a local account.
	Issuer  string
	Subject string
	// Username and Email are for display and lookup, never a join key.
	Username string
	Email    string
	// Groups are the group names the identity provider asserted, as asserted.
	Groups []string
	// Subjects are the binding subjects the caller acts under.
	Subjects []string
	Roles    []string
	// Admin passes every product rule and binding; the credential scope still applies. It is meant for break-glass callers: roles such as owner or admin are better ordinary roles, so a grant check can tell their holders apart. Mapping a role to Admin is the product's choice.
	Admin      bool
	Credential Credential
	// Audience is the `aud` the presented token was issued for, as the token carried it. Empty for any other credential and for a token without one. It lives here rather than on Credential so Credential stays comparable.
	Audience []string
	// SessionID correlates the session the caller came from. It is derived from the session token, never equal to it, because audit readers see it.
	SessionID string
	Status    Status
}

// Authenticated reports whether p is a caller that may be evaluated.
func (p *Principal) Authenticated() bool { return p != nil && (p.ID != "" || p.Admin) }

// Wildcard in a rule's kinds or verbs matches any kind or verb.
const Wildcard = "*"

// Rule grants every verb in Verbs on every kind in Kinds.
type Rule struct {
	Kinds []string
	Verbs []string
}

// Allows reports whether the rule covers (kind, verb).
func (r Rule) Allows(kind, verb string) bool {
	return covers(r.Kinds, kind) && covers(r.Verbs, verb)
}

func covers(set []string, want string) bool {
	for _, v := range set {
		if v == Wildcard || v == want {
			return true
		}
	}
	return false
}

// Scope is a place a binding attaches: the global scope, any resource that groups others, or a single resource.
type Scope struct {
	Kind string
	ID   string
}

// CredentialScope narrows a credential below what its principal holds. It fails closed: no rules permit nothing, and a kind present in Within with an empty list reaches nothing.
type CredentialScope struct {
	Rules []Rule
	// Within maps an action kind to the scopes the credential is limited to there: a resource of that kind is reached only when one of the listed scopes, of any kind, is in its scope chain. A list on a resource with no id and no owner is admitted, its rows then filtered through Visible or ScopeOf; any other verb on such a resource is refused. A kind absent from the map is not limited by scope.
	Within map[string][]Scope
}

type ctxKey struct{}

// With returns a context carrying p.
func With(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the principal in ctx, or nil when there is none.
func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

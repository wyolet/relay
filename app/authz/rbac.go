package authz

import (
	"context"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/rbac"
)

// catalogKinds are the shared template kinds every authenticated caller may
// read: they carry no tenant data and a deployment is unusable without them.
var catalogKinds = map[string]bool{
	"providers": true, "hosts": true, "models": true,
	"host-bindings": true, "pricings": true, "rate-limits": true,
}

// sharedCatalogKinds hold the rows that decide which upstream relay calls and
// what it charges. No tenant owns one, so writing them takes a role binding;
// owning the row grants nothing.
var sharedCatalogKinds = map[string]bool{
	"providers": true, "hosts": true, "models": true,
	"host-bindings": true, "pricings": true,
}

// SharedCatalogKind reports whether kind (the singular handlers pass) is
// shared catalog data that no user, team or project may own.
func SharedCatalogKind(kind string) bool { return sharedCatalogKinds[plural(kind)] }

// scopedKinds are the kinds whose lists are filtered row by row through
// Visible. Asking for such a list is safe for any authenticated caller: what
// comes back is what they may see, which for a caller with no binding at all
// is nothing. `users` is deliberately absent — a scoped caller must not
// enumerate the deployment's users.
var scopedKinds = map[string]bool{
	"keys": true, "policies": true, "host-keys": true, "service-accounts": true,
	"policy-bindings": true, "role-bindings": true, "projects": true,
	"teams": true, "groups": true,
}

// plurals maps the singular Resource.Kind handlers pass to the API plural a
// Role rule names. Kinds that are already plural (usage, logs, settings, …)
// map to themselves and are absent from the table.
var plurals = map[string]string{
	"provider":        "providers",
	"host":            "hosts",
	"model":           "models",
	"host-binding":    "host-bindings",
	"pricing":         "pricings",
	"rate-limit":      "rate-limits",
	"policy":          "policies",
	"host-key":        "host-keys",
	"key":             "keys",
	"team":            "teams",
	"project":         "projects",
	"group":           "groups",
	"role":            "roles",
	"role-binding":    "role-bindings",
	"policy-binding":  "policy-bindings",
	"service-account": "service-accounts",
	"user":            "users",
	"token":           "tokens",
}

// plural renders kind as the API plural a Role rule names.
func plural(kind string) string {
	if p, ok := plurals[kind]; ok {
		return p
	}
	return kind
}

// singulars is the reverse of plurals, so a caller holding only a URL path
// segment can name the kind handlers pass.
var singulars = func() map[string]string {
	out := make(map[string]string, len(plurals))
	for k, p := range plurals {
		out[p] = k
	}
	return out
}()

// Singular maps an API plural back to the Resource.Kind handlers set.
// Exported for app/audit, which reconstructs a resource from the request
// path when a handler refuses before authorizing.
func Singular(p string) string {
	if k, ok := singulars[p]; ok {
		return k
	}
	return p
}

// Snapshot is the slice of the catalog snapshot the evaluator reads.
// Declared here rather than imported: app/catalog reaches app/authz through
// the boot seed (catalog → seed → apply → authz), so importing it back would
// close a cycle. *catalog.Snapshot satisfies this as-is.
type Snapshot interface {
	// ScopeChainFor returns the scopes the identified row lives in, most
	// specific first, always ending in the global scope. A Team or Project
	// row is inside the scope it defines; every other kind resolves from
	// its owner alone.
	ScopeChainFor(kind, id string, o *meta.Owner) []meta.Owner
	// RoleBindingsForSubject returns the bindings naming a subject string.
	RoleBindingsForSubject(subject string) []*rolebinding.RoleBinding
	// ProjectsInTeam returns the team's projects, so a binding at a project
	// can answer for the team row above it.
	ProjectsInTeam(teamID string) []*project.Project
	// Role returns an enabled Role by id.
	Role(id string) (*role.Role, bool)
}

// RBAC is the role-based authorizer: a decision is the intersection of the
// actor's subjects, the RoleBindings naming them, and the scope chain of the
// row being touched. Default deny.
//
// Everything it reads comes from the in-memory catalog snapshot; Snap is
// catalog.Catalog.Current. The auth/rbac engine evaluates; relay's shortcuts are the product rules it is handed.
type RBAC struct{ Snap func() Snapshot }

// Authorize implements Authorizer.
func (r RBAC) Authorize(ctx context.Context, action string, res Resource) error {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return ErrUnauthenticated
	}
	ev := acquireEvaluation(r.Snap, a)
	defer ev.release()
	engine := ev.engine()
	return engine.Authorize(ctx, &ev.principal, action, ev.resource(res))
}

// Visible implements Scoper. Seeing one row is exactly the get verb on it,
// so list filtering and per-row reads can never disagree.
func (r RBAC) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return false
	}
	ev := acquireEvaluation(r.Snap, a)
	defer ev.release()
	engine := ev.engine()
	// The kind half of "<plural>.get", without building the string.
	actionKind, _ := rbac.SplitAction(plural(kind))
	return engine.Visible(ctx, &ev.principal, actionKind, ev.resource(Resource{Kind: kind, ID: id, Owner: &owner}))
}

// scopeOf is the engine's ScopeOf over relay's rules: the owned rows of kind the caller in ctx may perform verb on.
func (r RBAC) scopeOf(ctx context.Context, verb, kind string) (rbac.Filter, error) {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return rbac.Filter{}, ErrUnauthenticated
	}
	ev := acquireEvaluation(r.Snap, a)
	defer ev.release()
	engine := ev.engine()
	return engine.ScopeOf(ctx, &ev.principal, verb, kind)
}

func adminActor(a *actor.Actor) bool {
	return a.AdminToken || a.HasRole(user.RoleAdmin)
}

// IsAdmin reports whether the caller in ctx holds the bootstrap admin
// identity (the break-glass token or the admin role). Exported for the few
// call sites outside this package that must relax a rule for an operator —
// they are not authorization decisions and must not grow into one.
func IsAdmin(ctx context.Context) bool {
	a := actor.From(ctx)
	return a != nil && a.IsAuthenticated() && adminActor(a)
}

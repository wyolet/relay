package authz

import (
	"context"
	"strings"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
)

// catalogKinds are the shared template kinds every authenticated caller may
// read: they carry no tenant data and a deployment is unusable without them.
var catalogKinds = map[string]bool{
	"providers": true, "hosts": true, "models": true,
	"host-bindings": true, "pricings": true, "rate-limits": true,
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

// Singular maps an API plural back to the Resource.Kind handlers stamp.
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
	// Role returns an enabled Role by id.
	Role(id string) (*role.Role, bool)
}

// RBAC is the role-based authorizer: a decision is the intersection of the
// actor's subjects, the RoleBindings naming them, and the scope chain of the
// row being touched. Default deny.
//
// Everything it reads comes from the in-memory catalog snapshot; Snap is
// catalog.Catalog.Current.
type RBAC struct{ Snap func() Snapshot }

// Authorize implements Authorizer.
func (r RBAC) Authorize(ctx context.Context, action string, res Resource) error {
	a := actor.From(ctx)
	if !a.IsAuthenticated() {
		return ErrUnauthenticated
	}
	if adminActor(a) {
		return nil
	}
	kind, verb := splitAction(action)
	if res.Owner != nil && ownedBy(*res.Owner, a) {
		return nil // personal row: its owner holds every verb on it
	}
	if (verb == "get" || verb == "list") && catalogKinds[kind] &&
		(res.Owner == nil || isCatalogOwner(*res.Owner)) {
		return nil
	}
	// A system-owned Role is a shared rule set, not tenant data: creating a
	// binding means reading the role it names, and a scoped admin holds no
	// binding at the global scope the row lives in.
	if (verb == "get" || verb == "list") && kind == "roles" &&
		res.Owner != nil && res.Owner.Kind == meta.OwnerSystem {
		return nil
	}
	if r.Snap == nil {
		return ErrForbidden
	}
	snap := r.Snap()
	if snap == nil {
		return ErrForbidden
	}
	chain := snap.ScopeChainFor(res.Kind, res.ID, res.Owner)
	// A list call names no row; the rows it returns are filtered by Visible
	// afterwards, so any binding granting the verb admits the call.
	anyScope := verb == "list" && res.Owner == nil
	for _, subj := range a.Subjects {
		for _, b := range snap.RoleBindingsForSubject(subj) {
			if !anyScope && !inChain(chain, b.Spec.Scope) {
				continue
			}
			if role, ok := snap.Role(b.Spec.RoleID); ok && role.Allows(kind, verb) {
				return nil
			}
		}
	}
	return ErrForbidden
}

// Visible implements Scoper. Seeing one row is exactly the get verb on it,
// so list filtering and per-row reads can never disagree.
func (r RBAC) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	return r.Authorize(ctx, plural(kind)+".get", Resource{Kind: kind, ID: id, Owner: &owner}) == nil
}

func inChain(chain []meta.Owner, scope meta.Owner) bool {
	for _, o := range chain {
		if o.Kind == scope.Kind && o.ID == scope.ID {
			return true
		}
	}
	return false
}

func ownedBy(o meta.Owner, a *actor.Actor) bool {
	return o.Kind == meta.OwnerUser && o.ID != "" && o.ID == a.UserID
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

func isCatalogOwner(o meta.Owner) bool {
	switch o.Kind {
	case meta.OwnerSystem, meta.OwnerProvider, meta.OwnerHost:
		return true
	case meta.OwnerUser:
		// A user owner with no id names nobody: catalog rows shipped before
		// owners carried one read like the catalog rows they are, not as a
		// personal row hidden from everyone.
		return o.ID == ""
	}
	return false
}

// splitAction cuts "<plural>.<verb>" into its two halves. Sub-resource
// actions carry a middle segment ("models.overlay.get"): the kind is
// everything before the first dot, the verb everything after the last.
func splitAction(action string) (kind, verb string) {
	kind, verb = action, action
	if i := strings.IndexByte(action, '.'); i >= 0 {
		kind = action[:i]
	}
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		verb = action[i+1:]
	}
	return kind, verb
}

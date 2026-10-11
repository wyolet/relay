// Package rbac is a default-deny role-based authorization engine. A caller is allowed an action on a resource when the admin flag is set, a product rule allows it, or a binding naming one of the caller's subjects grants a role whose rules cover the action at a scope in the resource's scope chain. A credential scope, when the principal carries one, then narrows every allow.
//
// Kinds, verbs, subjects and scopes are opaque strings to the engine; only the "get" and "list" verbs and the "system:authenticated" subject carry a meaning, documented where they are used. Storage, the vocabulary a product allows in a role, and its ownership shortcuts are the product's: it hands the engine a Source, a ScopeResolver and ordered ProductRules.
package rbac

import (
	"slices"
	"strings"

	"github.com/wyolet/relay/auth/principal"
)

// Wildcard in a rule's kinds or verbs matches any kind or verb.
const Wildcard = principal.Wildcard

// SubjectAuthenticated is the reserved subject every authenticated principal matches, whether or not its Subjects list it.
const SubjectAuthenticated = "system:authenticated"

const (
	// VerbGet is the verb Visible checks: seeing a resource is reading it.
	VerbGet = "get"
	// VerbList on a resource with no owner is admitted by a binding at any scope; the caller filters the rows it returns through Visible or ScopeOf. Callers that cannot filter rows (e.g. a Kubernetes-style authorizer) must set Owner, or one scoped binding admits the whole collection.
	VerbList = "list"
)

// Scope is a place a binding attaches: the global scope, any resource that groups others, or a single resource. It and Rule are declared with the principal because a credential scope is made of both.
type Scope = principal.Scope

// Rule grants every verb in Verbs on every kind in Kinds.
type Rule = principal.Rule

// Role is a named rule set. It carries no scope; the binding supplies one.
type Role struct {
	ID    string
	Rules []Rule
}

// Allows reports whether any rule of the role covers (kind, verb).
func (r Role) Allows(kind, verb string) bool { return rulesAllow(r.Rules, kind, verb) }

// Binding grants RoleID at Scope to the subjects it names. Which subjects those are is the Source's index, not a field: the engine only ever asks for the bindings of one subject.
type Binding struct {
	RoleID string
	Scope  Scope
	// Origin is an opaque marker of where the binding came from (operator configuration, an applied manifest); the product reads it, the engine never does.
	Origin string
}

// MatchesSubject reports whether p acts under subject.
func MatchesSubject(p *principal.Principal, subject string) bool {
	if !p.Authenticated() {
		return false
	}
	return subject == SubjectAuthenticated || slices.Contains(p.Subjects, subject)
}

// SplitAction cuts "<kind>[.<sub>].<verb>" into kind (everything before the first dot) and verb (everything after the last). An action without a dot is both. The middle segment shares its parent kind's permission; a sub-resource that needs a permission of its own makes it part of the kind ("servers/status.update" → kind "servers/status").
func SplitAction(action string) (kind, verb string) {
	kind, verb = action, action
	if i := strings.IndexByte(action, '.'); i >= 0 {
		kind = action[:i]
	}
	if i := strings.LastIndexByte(action, '.'); i >= 0 {
		verb = action[i+1:]
	}
	return kind, verb
}

func rulesAllow(rules []Rule, kind, verb string) bool {
	for _, r := range rules {
		if r.Allows(kind, verb) {
			return true
		}
	}
	return false
}

// Package rbac is a default-deny role-based authorization engine. A caller is allowed an action on a resource when the admin flag is set, a product rule allows it, or a binding naming one of the caller's subjects grants a role whose rules cover the action at a scope in the resource's scope chain. A credential scope, when the principal carries one, then narrows every allow.
//
// Kinds, verbs, subjects and scopes are opaque strings to the engine; only the "get" and "list" verbs and the "system:authenticated" subject carry a meaning, documented where they are used. Storage, the vocabulary a product allows in a role, and its ownership shortcuts are the product's: it hands the engine a Source, a ScopeResolver and ordered ProductRules.
package rbac

import (
	"slices"
	"strings"
)

// Wildcard in a rule's kinds or verbs matches any kind or verb.
const Wildcard = "*"

// SubjectAuthenticated is the reserved subject every authenticated principal matches, whether or not its Subjects list it.
const SubjectAuthenticated = "system:authenticated"

const (
	// VerbGet is the verb Visible checks: seeing a resource is reading it.
	VerbGet = "get"
	// VerbList on a resource with no owner is admitted by a binding at any scope; the caller filters the rows it returns through Visible.
	VerbList = "list"
)

// Scope is a place a binding attaches: the global scope, any resource that groups others, or a single resource.
type Scope struct {
	Kind string
	ID   string
}

// Rule grants every verb in Verbs on every kind in Kinds.
type Rule struct {
	Kinds []string
	Verbs []string
}

// Allows reports whether the rule covers (kind, verb).
func (r Rule) Allows(kind, verb string) bool {
	return covers(r.Kinds, kind) && covers(r.Verbs, verb)
}

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

// Principal is what the engine reads about a caller.
type Principal struct {
	// ID is the caller's durable id. Empty, with Admin unset, means unauthenticated.
	ID string
	// Subjects are the binding subjects the caller acts under.
	Subjects []string
	// Admin passes every product rule and binding; Credential still applies.
	Admin bool
	// Credential narrows what this credential may do; nil leaves it unrestricted.
	Credential *CredentialScope
}

// Authenticated reports whether p is a caller the engine may evaluate.
func (p *Principal) Authenticated() bool { return p != nil && (p.ID != "" || p.Admin) }

// CredentialScope narrows a credential below what its principal holds. It fails closed: no rules permit nothing, and a kind present in IDs reaches only the listed ids, none when the list is empty, and never a resource that names no id.
type CredentialScope struct {
	Rules []Rule
	// IDs maps an action kind to the only resource ids the credential reaches. A kind absent from the map is not limited by id.
	IDs map[string][]string
}

func (c *CredentialScope) permits(kind, verb, id string) bool {
	if c == nil {
		return true
	}
	if !rulesAllow(c.Rules, kind, verb) {
		return false
	}
	ids, limited := c.IDs[kind]
	return !limited || (id != "" && slices.Contains(ids, id))
}

// MatchesSubject reports whether p acts under subject.
func MatchesSubject(p *Principal, subject string) bool {
	if !p.Authenticated() {
		return false
	}
	return subject == SubjectAuthenticated || slices.Contains(p.Subjects, subject)
}

// SplitAction cuts "<kind>[.<sub>].<verb>" into kind (everything before the first dot) and verb (everything after the last). An action without a dot is both.
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

func covers(set []string, want string) bool {
	for _, v := range set {
		if v == Wildcard || v == want {
			return true
		}
	}
	return false
}

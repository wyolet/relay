// Package role is the domain layer for the Role entity — a named set of
// (kinds, verbs) rules. A Role carries no scope: the scope comes from the
// RoleBinding that names it, so one Role is reusable at every level.
//
// Authorization is app/authz's concern; this package validates shape only
// and answers the pure Allows question the evaluator will ask.
package role

import (
	"fmt"
	"sort"

	"github.com/go-playground/validator/v10"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/rbac"
)

// Wildcard matches any kind or verb inside a rule.
const Wildcard = rbac.Wildcard

// Kinds is the closed set a rule may name: the control-API plurals plus
// the wildcard. Sorted — membership is a binary search.
var Kinds = []string{
	"*", "accounts", "audit", "budgets", "debug", "groups", "host-bindings", "host-keys", "hosts",
	"keys", "license", "logs", "master-key", "models", "policies",
	"policy-bindings", "pools", "pricings", "projects", "providers", "rate-limits",
	"role-bindings", "roles", "scim-clients", "secret-keys", "service-accounts", "settings",
	"system", "teams", "tokens", "usage", "users",
}

// Verbs is the closed set a rule may name. Wider than CRUD because the
// special endpoints (rotate, health, mint, …) need names of their own.
var Verbs = []string{
	"*", "apply", "attach", "create", "delete", "destroy", "detach", "generate",
	"get", "health", "list", "mint", "read", "reload", "revoke", "rewrap", "rotate",
	"snapshot", "update", "use",
}

func init() {
	if err := meta.Validator.RegisterValidation("rbackind", isKind); err != nil {
		panic("role: register rbackind validator: " + err.Error())
	}
	if err := meta.Validator.RegisterValidation("rbacverb", isVerb); err != nil {
		panic("role: register rbacverb validator: " + err.Error())
	}
}

func isKind(fl validator.FieldLevel) bool { return inSet(Kinds, fl.Field().String()) }
func isVerb(fl validator.FieldLevel) bool { return inSet(Verbs, fl.Field().String()) }

func inSet(set []string, v string) bool {
	i := sort.SearchStrings(set, v)
	return i < len(set) && set[i] == v
}

// Rule grants every verb in Verbs on every kind in Kinds.
type Rule struct {
	Kinds []string `json:"kinds" yaml:"kinds" validate:"required,min=1,dive,rbackind" doc:"Control-API kinds (plurals, such as policies or host-keys) the rule covers; * covers every kind."`
	Verbs []string `json:"verbs" yaml:"verbs" validate:"required,min=1,dive,rbacverb" doc:"Verbs the rule grants on those kinds, such as list, get, create, update, delete, rotate; * grants every verb."`
}

// Role is a named rule set. Owner is system for the built-ins and user for
// custom roles.
type Role struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the rules and the enabled flag.
type Spec struct {
	Rules   []Rule `json:"rules"             yaml:"rules"             validate:"required,min=1,dive" doc:"Rules the role grants; a request is allowed when any rule covers its kind and verb."`
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False makes bindings of this role grant nothing. Omitted means true."`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (r *Role) IsEnabled() bool { return r.Spec.Enabled == nil || *r.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is system (built-in) or user (custom).
func (r *Role) Validate() error {
	if err := meta.Validator.Struct(r); err != nil {
		return err
	}
	switch r.Meta.Owner.Kind {
	case meta.OwnerSystem, meta.OwnerUser:
	default:
		return fmt.Errorf("role %q: owner.kind must be system or user, got %q", r.Meta.Name, r.Meta.Owner.Kind)
	}
	return nil
}

// Allows reports whether any rule covers (kind, verb). A rule matches when
// its kinds hold kind or "*" and its verbs hold verb or "*".
func (r *Role) Allows(kind, verb string) bool {
	for _, rule := range r.Spec.Rules {
		if (rbac.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}).Allows(kind, verb) {
			return true
		}
	}
	return false
}

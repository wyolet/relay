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
)

// Wildcard matches any kind or verb inside a rule.
const Wildcard = "*"

// Kinds is the closed set a rule may name: the control-API plurals plus
// the wildcard. Sorted — membership is a binary search.
var Kinds = []string{
	"*", "audit", "debug", "groups", "host-bindings", "host-keys", "hosts",
	"keys", "license", "logs", "master-key", "models", "policies",
	"policy-bindings", "pricings", "projects", "providers", "rate-limits",
	"role-bindings", "roles", "service-accounts", "settings", "system",
	"teams", "tokens", "usage",
}

// Verbs is the closed set a rule may name. Wider than CRUD because the
// special endpoints (rotate, health, mint, …) need names of their own.
var Verbs = []string{
	"*", "apply", "attach", "create", "delete", "detach", "generate", "get",
	"health", "list", "mint", "read", "reload", "rotate", "snapshot", "update",
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
	Kinds []string `json:"kinds" yaml:"kinds" validate:"required,min=1,dive,rbackind"`
	Verbs []string `json:"verbs" yaml:"verbs" validate:"required,min=1,dive,rbacverb"`
}

// Role is a named rule set. Owner is system for the built-ins and user for
// custom roles.
type Role struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the rules and the enabled flag.
type Spec struct {
	Rules   []Rule `json:"rules"             yaml:"rules"             validate:"required,min=1,dive"`
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
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
		if covers(rule.Kinds, kind) && covers(rule.Verbs, verb) {
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

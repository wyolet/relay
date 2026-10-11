package role

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// ToDirectory is r as the directory stores it.
func (r *Role) ToDirectory() *directory.Role {
	var rules []rbac.Rule
	if r.Spec.Rules != nil {
		rules = make([]rbac.Rule, len(r.Spec.Rules))
		for i, rule := range r.Spec.Rules {
			rules[i] = rbac.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}
		}
	}
	return &directory.Role{
		Meta: meta.ToDirectory(r.Meta),
		Spec: directory.RoleSpec{Rules: rules, Enabled: r.Spec.Enabled},
	}
}

// FromDirectory is a directory row as a Role.
func FromDirectory(r *directory.Role) *Role {
	var rules []Rule
	if r.Spec.Rules != nil {
		rules = make([]Rule, len(r.Spec.Rules))
		for i, rule := range r.Spec.Rules {
			rules[i] = Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}
		}
	}
	return &Role{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{Rules: rules, Enabled: r.Spec.Enabled},
	}
}

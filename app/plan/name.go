package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// NameOf returns the slug of the input object ref names in v: a user's username, every other input kind's metadata name. Empty when the object is absent, and for overlays and derived kinds, which have no slug.
func NameOf(v *engine.View, ref engine.Ref) string {
	switch ref.Kind {
	case KindProvider:
		return metaName(engine.Get[provider.Provider](v, ref.ID), func(r *provider.Provider) string { return r.Meta.Name })
	case KindHost:
		return metaName(engine.Get[host.Host](v, ref.ID), func(r *host.Host) string { return r.Meta.Name })
	case KindModel:
		return metaName(engine.Get[model.Model](v, ref.ID), func(r *model.Model) string { return r.Meta.Name })
	case KindBinding:
		return metaName(engine.Get[binding.Binding](v, ref.ID), func(r *binding.Binding) string { return r.Meta.Name })
	case KindPricing:
		return metaName(engine.Get[pricing.Pricing](v, ref.ID), func(r *pricing.Pricing) string { return r.Meta.Name })
	case KindPolicy:
		return metaName(engine.Get[policy.Policy](v, ref.ID), func(r *policy.Policy) string { return r.Meta.Name })
	case KindPolicyBinding:
		return metaName(engine.Get[policybinding.PolicyBinding](v, ref.ID), func(r *policybinding.PolicyBinding) string { return r.Meta.Name })
	case KindRateLimit:
		return metaName(engine.Get[ratelimit.RateLimit](v, ref.ID), func(r *ratelimit.RateLimit) string { return r.Meta.Name })
	case KindBudget:
		return metaName(engine.Get[budget.Budget](v, ref.ID), func(r *budget.Budget) string { return r.Meta.Name })
	case KindPool:
		return metaName(engine.Get[pool.Pool](v, ref.ID), func(r *pool.Pool) string { return r.Meta.Name })
	case KindAccount:
		return metaName(engine.Get[account.Account](v, ref.ID), func(r *account.Account) string { return r.Meta.Name })
	case KindCredential:
		return metaName(engine.Get[hostkey.HostKey](v, ref.ID), func(r *hostkey.HostKey) string { return r.Meta.Name })
	case KindTeam:
		return metaName(engine.Get[team.Team](v, ref.ID), func(r *team.Team) string { return r.Meta.Name })
	case KindProject:
		return metaName(engine.Get[project.Project](v, ref.ID), func(r *project.Project) string { return r.Meta.Name })
	case KindServiceAccount:
		return metaName(engine.Get[serviceaccount.ServiceAccount](v, ref.ID), func(r *serviceaccount.ServiceAccount) string { return r.Meta.Name })
	case KindUser:
		return metaName(engine.Get[user.User](v, ref.ID), func(r *user.User) string { return r.Username })
	case KindKey:
		return metaName(engine.Get[key.Key](v, ref.ID), func(r *key.Key) string { return r.Meta.Name })
	case KindRole:
		return metaName(engine.Get[role.Role](v, ref.ID), func(r *role.Role) string { return r.Meta.Name })
	case KindRoleBinding:
		return metaName(engine.Get[rolebinding.RoleBinding](v, ref.ID), func(r *rolebinding.RoleBinding) string { return r.Meta.Name })
	case KindGroup:
		return metaName(engine.Get[group.Group](v, ref.ID), func(r *group.Group) string { return r.Meta.Name })
	}
	return ""
}

// OwnerOf returns the owner of the input object ref names in v. Users are treated as system-owned for visibility purposes; overlays and derived kinds return system owner.
func OwnerOf(v *engine.View, ref engine.Ref) meta.Owner {
	sys := meta.Owner{Kind: meta.OwnerSystem}
	switch ref.Kind {
	case KindProvider:
		return metaOwner(engine.Get[provider.Provider](v, ref.ID), func(r *provider.Provider) meta.Owner { return r.Meta.Owner }, sys)
	case KindHost:
		return metaOwner(engine.Get[host.Host](v, ref.ID), func(r *host.Host) meta.Owner { return r.Meta.Owner }, sys)
	case KindModel:
		return metaOwner(engine.Get[model.Model](v, ref.ID), func(r *model.Model) meta.Owner { return r.Meta.Owner }, sys)
	case KindBinding:
		return metaOwner(engine.Get[binding.Binding](v, ref.ID), func(r *binding.Binding) meta.Owner { return r.Meta.Owner }, sys)
	case KindPricing:
		return metaOwner(engine.Get[pricing.Pricing](v, ref.ID), func(r *pricing.Pricing) meta.Owner { return r.Meta.Owner }, sys)
	case KindPolicy:
		return metaOwner(engine.Get[policy.Policy](v, ref.ID), func(r *policy.Policy) meta.Owner { return r.Meta.Owner }, sys)
	case KindPolicyBinding:
		return metaOwner(engine.Get[policybinding.PolicyBinding](v, ref.ID), func(r *policybinding.PolicyBinding) meta.Owner { return r.Meta.Owner }, sys)
	case KindRateLimit:
		return metaOwner(engine.Get[ratelimit.RateLimit](v, ref.ID), func(r *ratelimit.RateLimit) meta.Owner { return r.Meta.Owner }, sys)
	case KindBudget:
		return metaOwner(engine.Get[budget.Budget](v, ref.ID), func(r *budget.Budget) meta.Owner { return r.Meta.Owner }, sys)
	case KindPool:
		return metaOwner(engine.Get[pool.Pool](v, ref.ID), func(r *pool.Pool) meta.Owner { return r.Meta.Owner }, sys)
	case KindAccount:
		return metaOwner(engine.Get[account.Account](v, ref.ID), func(r *account.Account) meta.Owner { return r.Meta.Owner }, sys)
	case KindCredential:
		return metaOwner(engine.Get[hostkey.HostKey](v, ref.ID), func(r *hostkey.HostKey) meta.Owner { return r.Meta.Owner }, sys)
	case KindTeam:
		return metaOwner(engine.Get[team.Team](v, ref.ID), func(r *team.Team) meta.Owner { return r.Meta.Owner }, sys)
	case KindProject:
		return metaOwner(engine.Get[project.Project](v, ref.ID), func(r *project.Project) meta.Owner { return r.Meta.Owner }, sys)
	case KindServiceAccount:
		return metaOwner(engine.Get[serviceaccount.ServiceAccount](v, ref.ID), func(r *serviceaccount.ServiceAccount) meta.Owner { return r.Meta.Owner }, sys)
	case KindUser:
		// Users are identity rows without a tenant owner; the "user" singular treats them as system-owned.
		return sys
	case KindKey:
		return metaOwner(engine.Get[key.Key](v, ref.ID), func(r *key.Key) meta.Owner { return r.Meta.Owner }, sys)
	case KindRole:
		return metaOwner(engine.Get[role.Role](v, ref.ID), func(r *role.Role) meta.Owner { return r.Meta.Owner }, sys)
	case KindRoleBinding:
		return metaOwner(engine.Get[rolebinding.RoleBinding](v, ref.ID), func(r *rolebinding.RoleBinding) meta.Owner { return r.Meta.Owner }, sys)
	case KindGroup:
		return metaOwner(engine.Get[group.Group](v, ref.ID), func(r *group.Group) meta.Owner { return r.Meta.Owner }, sys)
	}
	return sys
}

// KindSingular maps an engine kind to the control API's singular resource name used in authz decisions. Empty when the kind has no control API representation (derived kinds).
func KindSingular(kind engine.Kind) string {
	switch kind {
	case KindProvider:
		return "provider"
	case KindHost:
		return "host"
	case KindModel:
		return "model"
	case KindBinding:
		return "host-binding"
	case KindPricing:
		return "pricing"
	case KindPolicy:
		return "policy"
	case KindPolicyBinding:
		return "policy-binding"
	case KindRateLimit:
		return "rate-limit"
	case KindBudget:
		return "budget"
	case KindPool:
		return "pool"
	case KindAccount:
		return "account"
	case KindCredential:
		return "host-key"
	case KindTeam:
		return "team"
	case KindProject:
		return "project"
	case KindServiceAccount:
		return "service-account"
	case KindUser:
		return "user"
	case KindKey:
		return "key"
	case KindRole:
		return "role"
	case KindRoleBinding:
		return "role-binding"
	case KindGroup:
		return "group"
	}
	return ""
}

func metaName[T any](r *T, name func(*T) string) string {
	if r == nil {
		return ""
	}
	return name(r)
}

func metaOwner[T any](r *T, owner func(*T) meta.Owner, fallback meta.Owner) meta.Owner {
	if r == nil {
		return fallback
	}
	return owner(r)
}

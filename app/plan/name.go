package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
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

func metaName[T any](r *T, name func(*T) string) string {
	if r == nil {
		return ""
	}
	return name(r)
}

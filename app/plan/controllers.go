package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

type controllers struct{ cells *cell.Registry }

// singleton is the Watch key of a kind with one output, id "".
var singleton = []string{""}

func toSingleton[T any](*T) []string { return singleton }

// all returns the controllers parents first: within a round the engine runs them in this order, so one write settles the whole chain in a single pass.
func (c *controllers) all() []*engine.Controller {
	catalog := engine.NewController("catalog-index", KindCatalogIndex, c.catalogIndex)
	engine.Watch(catalog, toSingleton[provider.Provider])
	engine.Watch(catalog, toSingleton[host.Host])
	engine.Watch(catalog, toSingleton[model.Model])
	engine.Watch(catalog, toSingleton[pricing.Pricing])
	engine.Watch(catalog, toSingleton[binding.Binding])
	engine.Watch(catalog, toSingleton[overlay.Overlay])

	expanded := engine.NewController("expanded-policy", KindExpandedPolicy, c.expandedPolicy)
	engine.Watch(expanded, func(p *policy.Policy) []string { return []string{p.Meta.ID} })

	rung := engine.NewController("rung-policy", KindRungPolicy, c.rungPolicy)
	engine.Watch(rung, bindingSubjects)

	acct := engine.NewController("account-plan", KindAccountPlan, c.accountPlan)
	engine.Watch(acct, func(a *account.Account) []string { return []string{a.Meta.ID} })

	slot := engine.NewController("key-slot", KindKeySlot, c.keySlot)
	engine.Watch(slot, func(k *hostkey.HostKey) []string { return []string{k.Meta.ID} })

	pl := engine.NewController("pool-plan", KindPoolPlan, c.poolPlan)
	engine.Watch(pl, func(p *pool.Pool) []string { return []string{p.Meta.ID} })

	owned := engine.NewController("owned-credentials", KindOwnedCredentials, c.ownedCredentials)
	engine.Watch(owned, func(k *hostkey.HostKey) []string { return []string{ownerRef(k.Meta.Owner)} })

	caller := engine.NewController("caller-plan", KindCallerPlan, c.callerPlan)
	engine.Watch(caller, func(sa *serviceaccount.ServiceAccount) []string { return []string{"serviceaccount:" + sa.Meta.ID} })
	engine.Watch(caller, func(u *user.User) []string { return []string{"user:" + u.ID} })

	kp := engine.NewController("key-plan", KindKeyPlan, c.keyPlan)
	engine.Watch(kp, func(k *key.Key) []string { return []string{k.Meta.ID} })

	shard := engine.NewController("key-plan-shard", KindKeyPlanShard, c.keyPlanShard)
	engine.Watch(shard, keyShards)

	index := engine.NewController("key-plan-index", KindKeyPlanIndex, c.keyPlanIndex)
	engine.Watch(index, toSingleton[KeyPlanShard])

	up := engine.NewController("user-plan", KindUserPlan, c.userPlan)
	engine.Watch(up, func(u *user.User) []string { return []string{u.ID} })

	users := engine.NewController("user-index", KindUserIndex, c.userIndex)
	engine.Watch(users, toSingleton[UserPlan])

	projects := engine.NewController("project-index", KindProjectIndex, c.projectIndex)
	engine.Watch(projects, toSingleton[project.Project])

	return []*engine.Controller{catalog, expanded, rung, acct, slot, pl, owned, caller, kp, shard, index, up, users, projects}
}

// Reads and admin-only writes of the personal-account switch on users and groups. The effective state comes from the published plans, which fold it from the user, their groups and the accounts settings section.

package control

import (
	"context"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/engine"
)

// PersonalAccountEffective is a user's personal-account state and what decided it.
type PersonalAccountEffective struct {
	State   string `json:"state"             enum:"on,off" doc:"Whether the user's personal account serves: on or off. Off refuses keys in it and the provider accounts the user owns personally with personal_account_off."`
	Source  string `json:"source"            enum:"user,group,setting" doc:"What decided it: the user's own personalAccount, a group they are in (groupId), or the accounts settings section."`
	GroupID string `json:"groupId,omitempty" doc:"The group that decided it, for source group."`
}

// personalAccountState is userID's state in the published plans; nil before the first publish or for an unknown user.
func personalAccountState(d Deps, userID string) *plan.PersonalAccount {
	if d.PlanView == nil || userID == "" {
		return nil
	}
	v := d.PlanView()
	if v == nil {
		return nil
	}
	return plan.PersonalAccountOf(v, userID)
}

// personalAccountEffective renders userID's state for a read; nil when the plans do not know it yet.
func personalAccountEffective(d Deps, userID string) *PersonalAccountEffective {
	p := personalAccountState(d, userID)
	if p == nil {
		return nil
	}
	out := &PersonalAccountEffective{State: "on", Source: string(p.Source), GroupID: p.GroupID}
	if !p.On {
		out.State = "off"
	}
	return out
}

// personalAccountEffectiveFor is userID's state with userValue as their own value, folded from the groups and setting the plans hold now, so a write's answer shows the value it saved before the plans rebuild; nil before the first publish.
func personalAccountEffectiveFor(d Deps, userID, userValue string) *PersonalAccountEffective {
	if d.PlanView == nil {
		return nil
	}
	v := d.PlanView()
	if v == nil {
		return nil
	}
	var groups []*group.Group
	for g := range engine.All[group.Group](v) {
		if slices.Contains(g.Spec.MemberIDs, userID) {
			groups = append(groups, g)
		}
	}
	on, source, groupID := plan.EffectivePersonalAccount(userValue, groups, engine.Get[settings.Accounts](v, settings.SectionAccounts))
	out := &PersonalAccountEffective{State: "on", Source: string(source), GroupID: groupID}
	if !on {
		out.State = "off"
	}
	return out
}

// guardGroupPersonalAccount leaves a group's personalAccount to admins, since it decides whether other users may act on their own: only an admin sets it, it sits only on system groups, and while a group carries it only an admin changes the group's members, enabled flag, name or owner, or deletes it (incoming nil). A non-admin's write that leaves it out keeps the stored value.
func guardGroupPersonalAccount(ctx context.Context, az authz.Authorizer, existing, incoming *group.Group) error {
	var stored string
	if existing != nil {
		stored = existing.Spec.PersonalAccount
	}
	admin := refcheck.RequireAdmin(ctx, az, "a group that turns personal accounts on or off")
	if incoming == nil {
		if stored != "" && admin != nil {
			return refErr(admin)
		}
		return nil
	}
	if incoming.Spec.PersonalAccount == "" && admin != nil {
		incoming.Spec.PersonalAccount = stored
	}
	switch {
	case admin == nil:
	case incoming.Spec.PersonalAccount != stored:
		return refErr(refcheck.RequireAdmin(ctx, az, "a group's personalAccount"))
	case stored != "" && !group.SameExceptPersonalAccount(existing, incoming):
		return refErr(admin)
	}
	if err := incoming.PersonalAccountOwnerRule(); err != nil {
		return huma.Error400BadRequest(err.Error())
	}
	return nil
}

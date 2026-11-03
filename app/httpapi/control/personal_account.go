// Reads and admin-only writes of the personal-account switch on users and groups. The effective state comes from the published plans, which fold it from the user, their groups and the accounts settings section.

package control

import (
	"context"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/refcheck"
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

// guardGroupPersonalAccount leaves a group's personalAccount to admins: it decides whether other users may act on their own, and a group may be owned by a user who is no admin. A non-admin's write that leaves it out keeps the stored value.
func guardGroupPersonalAccount(ctx context.Context, az authz.Authorizer, existing, incoming *group.Group) error {
	var stored string
	if existing != nil {
		stored = existing.Spec.PersonalAccount
	}
	if incoming.Spec.PersonalAccount == stored || refcheck.RequireAdmin(ctx, az, "a group's personalAccount") == nil {
		return nil
	}
	if incoming.Spec.PersonalAccount == "" {
		incoming.Spec.PersonalAccount = stored
		return nil
	}
	return refErr(refcheck.RequireAdmin(ctx, az, "a group's personalAccount"))
}

package plan_test

import (
	"testing"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
)

// A project-owned binding on a user's member account there governs that lane only: not the user's personal key, and not the user's lane in another project of the same team.
func TestBindingOwner_ProjectBindingOnAMemberAccountGovernsThatLane(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	other := o.w.Project(o.team)
	u := o.w.User()
	inProject := o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
	o.w.Member(u, ownedBy(meta.OwnerProject, other.Meta.ID))
	k := o.w.UserKey(u, "sk-alice")
	pol := o.w.Policy("eval-only-m1", policy.Spec{Models: []string{"prov/m1"}})
	pol.Meta.Owner = ownedBy(meta.OwnerProject, o.proj.Meta.ID)
	pb := o.w.BindPolicy(pol, inProject)
	pb.Meta.Owner = ownedBy(meta.OwnerProject, o.proj.Meta.ID)
	plans := o.w.Load(t)

	lane := tokenCaller(t, plans, u.ID, o.proj.Meta.ID)
	mustRoute(t, lane, "m1")
	wantRefusal(t, lane.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungLane, pol.Meta.ID)

	mustRoute(t, keyCaller(t, plans, k), "m2")
	mustRoute(t, tokenCaller(t, plans, u.ID, other.Meta.ID), "m2")
}

// A team-owned binding on the team's own account governs every lane under the team, and nowhere else.
func TestBindingOwner_TeamAccountBindingGovernsEveryLaneUnderIt(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	sibling := o.w.Project(o.team)
	elsewhere := o.w.Project(o.w.Team())
	u := o.w.User()
	for _, p := range []meta.Owner{ownedBy(meta.OwnerProject, o.proj.Meta.ID), ownedBy(meta.OwnerProject, sibling.Meta.ID), ownedBy(meta.OwnerProject, elsewhere.Meta.ID)} {
		o.w.Member(u, p)
	}
	k := o.w.UserKey(u, "sk-alice")
	pol := o.w.Policy("team-only-m1", policy.Spec{Models: []string{"prov/m1"}})
	pol.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	pb := o.w.BindPolicy(pol, o.teamAcct())
	pb.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	plans := o.w.Load(t)

	for _, p := range []string{o.proj.Meta.ID, sibling.Meta.ID} {
		wantRefusal(t, tokenCaller(t, plans, u.ID, p).Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungTeam, pol.Meta.ID)
	}
	wantRefusal(t, keyCaller(t, plans, o.saKey).Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungTeam, pol.Meta.ID)
	mustRoute(t, tokenCaller(t, plans, u.ID, elsewhere.Meta.ID), "m2")
	mustRoute(t, keyCaller(t, plans, k), "m2")
}

// A system-owned binding on a user's personal account governs the personal lane alone.
func TestBindingOwner_PersonalAccountBindingGovernsThePersonalLane(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	u := o.w.User()
	o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
	k := o.w.UserKey(u, "sk-alice")
	pol := o.w.Policy("only-m1", policy.Spec{Models: []string{"prov/m1"}})
	o.w.BindPolicy(pol, o.w.PersonalAccount(u))
	plans := o.w.Load(t)

	wantRefusal(t, keyCaller(t, plans, k).Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungLane, pol.Meta.ID)
	mustRoute(t, tokenCaller(t, plans, u.ID, o.proj.Meta.ID), "m2")
}

// A binding takes effect only for accounts inside its owner's scope: a project-owned binding naming another project's service account's account does not govern that account's requests.
func TestBindingOwner_ProjectBindingOutsideItsScopeDoesNotApply(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	other := o.w.Project(o.team)
	pol := o.w.Policy("other-project", policy.Spec{Models: []string{"prov/m1"}})
	pol.Meta.Owner = ownedBy(meta.OwnerProject, other.Meta.ID)
	pb := o.w.BindPolicy(pol, o.saAcct())
	pb.Meta.Owner = ownedBy(meta.OwnerProject, other.Meta.ID)
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	if r := cp.Routes.Get("m2"); r.Refused != nil {
		t.Fatalf("the service account is governed by a binding its project does not own: %v", r.Refused)
	}
}

// A tier preset is a host-owned policy any binding may use: a team binds it to the team's provider account and it narrows that account's credentials.
func TestBindingOwner_HostOwnedTierPresetBoundByTeamToItsAccount(t *testing.T) {
	o := newOrg()
	acct := o.w.Account(o.host)
	acct.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	o.w.Credential(o.host, acct, ownedBy(meta.OwnerTeam, o.team.Meta.ID), "sk-team")
	tier := o.w.Policy("tier-1", policy.Spec{Models: []string{"prov/m1"}})
	tier.Meta.Owner = ownedBy(meta.OwnerHost, o.host.Meta.ID)
	pb := o.w.BindPolicy(tier, acct)
	pb.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	mustRoute(t, cp, "m1")
	wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungAccount, tier.Meta.ID)
}

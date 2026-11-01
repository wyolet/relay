package plan_test

import (
	"testing"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

// A project-owned binding on a user governs only that user's tokens for that project: not the user's personal keys, and not tokens for another project of the same team.
func TestBindingOwner_ProjectBindingOnUserGovernsOnlyThatProjectsTokens(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	other := o.w.Project(o.team)
	u := o.w.User()
	k := o.w.UserKey(u, "sk-alice")
	pol := o.w.Policy("eval-only-m1", policy.Spec{Models: []string{"prov/m1"}})
	pol.Meta.Owner = ownedBy(meta.OwnerProject, o.proj.Meta.ID)
	pb := o.w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	pb.Spec.ProjectID = o.proj.Meta.ID
	pb.StampOwner()
	plans := o.w.Load(t)

	inProject := tokenCaller(t, plans, u.ID, o.proj.Meta.ID)
	mustRoute(t, inProject, "m1")
	wantRefusal(t, inProject.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungPrincipal, pol.Meta.ID)

	mustRoute(t, keyCaller(t, plans, k), "m2")
	mustRoute(t, tokenCaller(t, plans, u.ID, other.Meta.ID), "m2")
}

// A team-owned binding on a user governs the user's tokens in every project of that team, and nowhere else.
func TestBindingOwner_TeamBindingOnUserGovernsTokensInItsProjects(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	sibling := o.w.Project(o.team)
	elsewhere := o.w.Project(o.w.Team())
	u := o.w.User()
	k := o.w.UserKey(u, "sk-alice")
	pol := o.w.Policy("team-only-m1", policy.Spec{Models: []string{"prov/m1"}})
	pol.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	pb := o.w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	pb.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	plans := o.w.Load(t)

	for _, p := range []string{o.proj.Meta.ID, sibling.Meta.ID} {
		cp := tokenCaller(t, plans, u.ID, p)
		wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungPrincipal, pol.Meta.ID)
	}
	mustRoute(t, tokenCaller(t, plans, u.ID, elsewhere.Meta.ID), "m2")
	mustRoute(t, keyCaller(t, plans, k), "m2")
}

// A system-owned binding on a user governs the user everywhere: personal keys and tokens for any project.
func TestBindingOwner_SystemBindingOnUserGovernsEverywhere(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	u := o.w.User()
	k := o.w.UserKey(u, "sk-alice")
	pol := o.w.Policy("only-m1", policy.Spec{Models: []string{"prov/m1"}})
	o.w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	plans := o.w.Load(t)

	for _, cp := range []*plan.CallerPlan{keyCaller(t, plans, k), tokenCaller(t, plans, u.ID, o.proj.Meta.ID)} {
		wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungPrincipal, pol.Meta.ID)
	}
}

// A binding takes effect only for requests inside its owner's scope: a project-owned binding naming another project's service account does not govern that account's requests.
func TestBindingOwner_ProjectBindingOutsideItsScopeDoesNotApply(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	other := o.w.Project(o.team)
	pol := o.w.Policy("other-project", policy.Spec{Models: []string{"prov/m1"}})
	pol.Meta.Owner = ownedBy(meta.OwnerProject, other.Meta.ID)
	pb := o.w.BindPolicy(pol, saSubject(o.sa))
	pb.Spec.ProjectID = other.Meta.ID
	pb.StampOwner()
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	if r := cp.Routes.Get("m2"); r.Refused != nil {
		t.Fatalf("the service account is governed by a binding its project does not own: %v", r.Refused)
	}
}

// A tier preset is a host-owned policy any binding may use: a team binds it to the team's own account and it narrows that account's credentials.
func TestBindingOwner_HostOwnedTierPresetBoundByTeamToItsAccount(t *testing.T) {
	o := newOrg()
	acct := o.w.Account(o.host)
	acct.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	o.w.Credential(o.host, acct, ownedBy(meta.OwnerTeam, o.team.Meta.ID), "sk-team")
	tier := o.w.Policy("tier-1", policy.Spec{Models: []string{"prov/m1"}})
	tier.Meta.Owner = ownedBy(meta.OwnerHost, o.host.Meta.ID)
	pb := o.w.BindPolicy(tier, plantest.Subject(policybinding.SubjectAccount, acct.Meta.ID))
	pb.Meta.Owner = ownedBy(meta.OwnerTeam, o.team.Meta.ID)
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	mustRoute(t, cp, "m1")
	wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungAccount, tier.Meta.ID)
}

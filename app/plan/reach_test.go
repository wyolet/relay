package plan_test

import (
	"testing"

	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/meta"
)

// Without a caller policy a service account reaches the credentials its own scopes own (its project's, its team's) and the system's, innermost owner first, and nothing another scope owns.
func TestReach_WithoutCallerPolicyServiceAccountReachesOwnScopesAndSystem(t *testing.T) {
	o := newOrg()
	sys := o.w.Credential(o.host, nil, plantest.System, "sk-system")
	tm := o.w.Credential(o.host, nil, ownedBy(meta.OwnerTeam, o.team.Meta.ID), "sk-team")
	pr := o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	sibling := o.w.Project(o.team)
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, sibling.Meta.ID), "sk-sibling-project")
	otherTeam := o.w.Team()
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerTeam, otherTeam.Meta.ID), "sk-other-team")
	someone := o.w.User()
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerUser, someone.ID), "sk-someone")
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	want := []string{pr.Meta.Name, tm.Meta.Name, sys.Meta.Name}
	if got := routeCredentials(mustRoute(t, cp, "m1")); !sameList(got, want) {
		t.Fatalf("route reaches %v, want %v (project, team, system)", got, want)
	}
}

// Without a caller policy a personal key reaches its user's own credentials and the system's, nothing another user or a team owns.
func TestReach_WithoutCallerPolicyUserReachesOwnAndSystem(t *testing.T) {
	o := newOrg()
	sys := o.w.Credential(o.host, nil, plantest.System, "sk-system")
	u := o.w.User()
	own := o.w.Credential(o.host, nil, ownedBy(meta.OwnerUser, u.ID), "sk-own")
	other := o.w.User()
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerUser, other.ID), "sk-other-user")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerTeam, o.team.Meta.ID), "sk-team")
	k := o.w.UserKey(u, "sk-personal")
	cp := keyCaller(t, o.w.Load(t), k)

	want := []string{own.Meta.Name, sys.Meta.Name}
	if got := routeCredentials(mustRoute(t, cp, "m1")); !sameList(got, want) {
		t.Fatalf("route reaches %v, want %v", got, want)
	}
}

// A caller policy that links nothing (only models or caps) leaves the reach by ownership as it is: an empty list does not constrain.
func TestReach_PolicyWithoutLinksKeepsOwnershipReach(t *testing.T) {
	o := newOrg()
	sys := o.w.Credential(o.host, nil, plantest.System, "sk-system")
	pr := o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	pol := o.w.Policy("models-only", policy.Spec{Models: []string{"prov/m1"}})
	o.w.BindPolicy(pol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	want := []string{pr.Meta.Name, sys.Meta.Name}
	if got := routeCredentials(mustRoute(t, cp, "m1")); !sameList(got, want) {
		t.Fatalf("route reaches %v, want %v", got, want)
	}
}

// A caller policy that links a pool reaches only what it links: the credentials the principal's scopes own drop out.
func TestReach_CallerPolicyReachesOnlyWhatItLinks(t *testing.T) {
	o := newOrg()
	linked := o.w.Credential(o.host, nil, plantest.System, "sk-linked")
	o.w.Credential(o.host, nil, plantest.System, "sk-unlinked-system")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	p := o.w.Pool(o.host, linked)
	pol := o.w.Policy("linker", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	o.w.BindPolicy(pol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	if got := routeCredentials(mustRoute(t, cp, "m1")); !sameList(got, []string{linked.Meta.Name}) {
		t.Fatalf("route reaches %v, want only %s", got, linked.Meta.Name)
	}
}

// The innermost rung that links pools gives their order; outer rungs that link pools only intersect, never add or reorder.
func TestReach_PoolsInInnermostOrderOuterRungsIntersect(t *testing.T) {
	o := newOrg()
	c1 := o.w.Credential(o.host, nil, plantest.System, "sk-1")
	c2 := o.w.Credential(o.host, nil, plantest.System, "sk-2")
	c3 := o.w.Credential(o.host, nil, plantest.System, "sk-3")
	c4 := o.w.Credential(o.host, nil, plantest.System, "sk-4")
	p1, p2, p3, p4 := o.w.Pool(o.host, c1), o.w.Pool(o.host, c2), o.w.Pool(o.host, c3), o.w.Pool(o.host, c4)
	projPol := o.w.Policy("project", policy.Spec{PoolIDs: []string{p1.Meta.ID, p2.Meta.ID, p3.Meta.ID}})
	saPol := o.w.Policy("sa", policy.Spec{PoolIDs: []string{p3.Meta.ID, p1.Meta.ID, p4.Meta.ID}})
	o.w.BindPolicy(projPol, o.projectAcct())
	o.w.BindPolicy(saPol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	want := []string{c3.Meta.Name, c1.Meta.Name}
	if got := callerCredentials(cp); !sameList(got, want) {
		t.Fatalf("caller pools hold %v, want %v (inner order, outer intersect)", got, want)
	}
	if got := routeCredentials(mustRoute(t, cp, "m1")); !sameList(got, want) {
		t.Fatalf("route reaches %v, want %v", got, want)
	}
}

// When only an outer rung links pools, the inner rungs inherit its reach.
func TestReach_OuterRungLinksApplyWhenInnerLinksNothing(t *testing.T) {
	o := newOrg()
	c1 := o.w.Credential(o.host, nil, plantest.System, "sk-1")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	p1 := o.w.Pool(o.host, c1)
	teamPol := o.w.Policy("team", policy.Spec{PoolIDs: []string{p1.Meta.ID}})
	saPol := o.w.Policy("sa", policy.Spec{Models: []string{"prov/m1"}})
	o.w.BindPolicy(teamPol, o.teamAcct())
	o.w.BindPolicy(saPol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	if got := routeCredentials(mustRoute(t, cp, "m1")); !sameList(got, []string{c1.Meta.Name}) {
		t.Fatalf("route reaches %v, want only %s", got, c1.Meta.Name)
	}
}

// Pool order in the policy is the cross-host failover order: a model served at two hosts is tried at the first pool's host first.
func TestReach_PoolOrderIsCrossHostFailoverOrder(t *testing.T) {
	o := newOrg()
	second := o.w.Host("h2", "http://h2.invalid")
	o.w.Bind(o.models["m1"], second, adapters.OpenAI)
	o.w.Price(o.models["m1"], second, 1)
	a := o.w.Credential(o.host, nil, plantest.System, "sk-h1")
	b := o.w.Credential(second, nil, plantest.System, "sk-h2")
	pa, pb := o.w.Pool(o.host, a), o.w.Pool(second, b)
	pol := o.w.Policy("failover", policy.Spec{PoolIDs: []string{pb.Meta.ID, pa.Meta.ID}})
	o.w.BindPolicy(pol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	r := mustRoute(t, cp, "m1")
	if len(r.Hops) != 2 || r.Hops[0].Host.Meta.ID != second.Meta.ID || r.Hops[1].Host.Meta.ID != o.host.Meta.ID {
		t.Fatalf("hops = %v, want h2 then h", routeCredentials(r))
	}
}

// A keyless host is reached only through a pool at it: neither ownership nor a wildcard model grant reaches it.
func TestReach_KeylessHostOnlyThroughAPool(t *testing.T) {
	build := func(link bool, models []string) *plan.CallerPlan {
		o := newOrg()
		free := o.w.Host("free", "http://free.invalid")
		free.Spec.NoAuth = true
		m := o.w.Model(o.prov, "local")
		o.w.Bind(m, free, adapters.OpenAI)
		o.w.Price(m, free, 1)
		spec := policy.Spec{Models: models}
		if link {
			spec.PoolIDs = []string{o.w.Pool(free).Meta.ID}
		}
		if link || models != nil {
			o.w.BindPolicy(o.w.Policy("p", spec), o.saAcct())
		}
		return keyCaller(t, o.w.Load(t), o.saKey)
	}
	if r := build(false, nil).Routes.Get("local"); r.Refused == nil {
		t.Error("a keyless host was reached with no policy at all")
	}
	if r := build(false, []string{"prov/*"}).Routes.Get("local"); r.Refused == nil {
		t.Error("a wildcard model grant reached a keyless host")
	}
	if r := build(true, nil).Routes.Get("local"); r.Refused != nil || len(r.Hops) == 0 {
		t.Errorf("a pool at the keyless host did not reach it: %v", r.Refused)
	}
}

package plan_test

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// A policy on any one caller rung narrows the models the principal may call, and a refused model names that rung and its policy.
func TestLadder_EachCallerRungNarrowsTheModels(t *testing.T) {
	for _, rung := range []limits.Rung{limits.RungTeam, limits.RungProject, limits.RungLane} {
		t.Run(string(rung), func(t *testing.T) {
			o := newOrg()
			o.w.Credential(o.host, nil, plantest.System, "sk-system")
			pol := o.w.Policy("only-m1", policy.Spec{Models: []string{"prov/m1"}})
			switch rung {
			case limits.RungTeam:
				o.w.BindPolicy(pol, o.teamAcct())
			case limits.RungProject:
				o.w.BindPolicy(pol, o.projectAcct())
			default:
				o.w.BindPolicy(pol, o.saAcct())
			}
			cp := keyCaller(t, o.w.Load(t), o.saKey)
			mustRoute(t, cp, "m1")
			wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, rung, pol.Meta.ID)
		})
	}
}

// Policies on several caller rungs intersect: a model passes only when every rung allows it, and the refusal names the one rung that did not.
func TestLadder_CallerRungsIntersect(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	teamPol := o.w.Policy("team", policy.Spec{Models: []string{"prov/m1", "prov/m2", "prov/m3"}})
	projPol := o.w.Policy("project", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	saPol := o.w.Policy("sa", policy.Spec{Models: []string{"prov/m2", "prov/m3"}})
	o.w.BindPolicy(teamPol, o.teamAcct())
	o.w.BindPolicy(projPol, o.projectAcct())
	o.w.BindPolicy(saPol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	mustRoute(t, cp, "m2")
	wantRefusal(t, cp.Routes.Get("m1"), plan.CodeModelNotAllowed, limits.RungLane, saPol.Meta.ID)
	wantRefusal(t, cp.Routes.Get("m3"), plan.CodeModelNotAllowed, limits.RungProject, projPol.Meta.ID)
}

// A rung with no policy bound does not constrain: with only the project bound, everything the project allows passes and the rest is refused on its rung.
func TestLadder_RungWithoutPolicyDoesNotConstrain(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	pol := o.w.Policy("project", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	o.w.BindPolicy(pol, o.projectAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	if cp.Team().Policy != nil || cp.LaneRung().Policy != nil {
		t.Fatalf("unbound rungs carry a policy: team %v, lane %v", cp.Team().Policy, cp.LaneRung().Policy)
	}
	mustRoute(t, cp, "m1")
	mustRoute(t, cp, "m2")
	wantRefusal(t, cp.Routes.Get("m3"), plan.CodeModelNotAllowed, limits.RungProject, pol.Meta.ID)
}

// An inner policy with an empty model list inherits the outer rung's grant; it never widens it.
func TestLadder_EmptyModelListInheritsTheOuterRung(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	teamPol := o.w.Policy("team", policy.Spec{Models: []string{"prov/m1"}})
	rl := o.w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 100, time.Minute))
	saPol := o.w.Policy("sa-limit-only", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	o.w.BindPolicy(teamPol, o.teamAcct())
	o.w.BindPolicy(saPol, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	mustRoute(t, cp, "m1")
	wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungTeam, teamPol.Meta.ID)
}

// The provider account's policy narrows the models its credentials may serve, a host-owned tier preset as much as any; a model it refuses is refused naming the account rung and the policy.
func TestLadder_TheProviderAccountNarrowsTheModels(t *testing.T) {
	for _, preset := range []bool{false, true} {
		o := newOrg()
		acct := o.w.Account(o.host)
		o.w.Credential(o.host, acct, plantest.System, "sk-acct")
		pol := o.w.Policy("tier", policy.Spec{Models: []string{"prov/m1"}})
		if preset {
			pol.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: o.host.Meta.ID}
		}
		o.w.BindPolicy(pol, acct)
		cp := keyCaller(t, o.w.Load(t), o.saKey)
		mustRoute(t, cp, "m1")
		wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungAccount, pol.Meta.ID)
	}
}

// Upstream and caller rungs combine: the caller may only call what both sides allow.
func TestLadder_UpstreamAndCallerSidesIntersect(t *testing.T) {
	o := newOrg()
	acct := o.w.Account(o.host)
	o.w.Credential(o.host, acct, plantest.System, "sk-acct")
	tier := o.w.Policy("tier", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	o.w.BindPolicy(tier, acct)
	caller := o.w.Policy("caller", policy.Spec{Models: []string{"prov/m2", "prov/m3"}})
	o.w.BindPolicy(caller, o.saAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	mustRoute(t, cp, "m2")
	wantRefusal(t, cp.Routes.Get("m1"), plan.CodeModelNotAllowed, limits.RungLane, caller.Meta.ID)
	wantRefusal(t, cp.Routes.Get("m3"), plan.CodeModelNotAllowed, limits.RungAccount, tier.Meta.ID)
}

// A disabled upstream policy takes the credentials under it out instead of letting them serve unmetered: a credential outside the account keeps serving, and with none the route is refused naming the rung and the policy.
func TestLadder_DisabledUpstreamPolicyTakesOutItsCredentials(t *testing.T) {
	for _, outside := range []bool{false, true} {
		o := newOrg()
		acct := o.w.Account(o.host)
		o.w.Credential(o.host, acct, plantest.System, "sk-acct")
		var want []string
		if outside {
			want = []string{o.w.Credential(o.host, nil, plantest.System, "sk-free").Meta.Name}
		}
		tier := o.w.Policy("tier-off", policy.Spec{Enabled: off()})
		o.w.BindPolicy(tier, acct)
		r := keyCaller(t, o.w.Load(t), o.saKey).Routes.Get("m1")
		if !outside {
			wantRefusal(t, r, plan.CodePolicyDisabled, limits.RungAccount, tier.Meta.ID)
		} else if got := routeCredentials(r); r.Refused != nil || !slices.Equal(got, want) {
			t.Fatalf("route reaches %v (refusal %v), want only %v", got, r.Refused, want)
		}
	}
}

// One policy may govern many accounts: binding it to two service accounts' accounts governs both.
func TestLadder_OnePolicyGovernsManySubjects(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	other := o.w.ServiceAccount(o.proj)
	otherKey := o.w.ServiceAccountKey(other, "sk-other")
	pol := o.w.Policy("shared", policy.Spec{Models: []string{"prov/m1"}})
	o.w.BindPolicy(pol, o.saAcct(), o.w.ServiceAccountAccount(other))
	plans := o.w.Load(t)

	for _, cp := range []*plan.CallerPlan{keyCaller(t, plans, o.saKey), keyCaller(t, plans, otherKey)} {
		mustRoute(t, cp, "m1")
		wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungLane, pol.Meta.ID)
	}
}

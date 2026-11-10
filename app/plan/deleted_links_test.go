package plan_test

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

func countersOf(r *plan.Route, kind limits.Kind) int {
	n := 0
	for _, c := range r.Hops[0].CallerCounters() {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

// Deleting a budget a policy links drops only that cap: the policy keeps its models and other caps, as if it never linked the budget, and carries a ref_missing issue.
func TestDeletedBudget_PolicyLosesOnlyThatCap(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	b := o.w.Budget("10")
	rl := o.w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 10, time.Minute))
	pol := o.w.Policy("capped", policy.Spec{Models: []string{"prov/m1"}, Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID}}, RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	o.w.BindPolicy(pol, o.saAcct())
	plans := o.w.Load(t)
	before := mustRoute(t, keyCaller(t, plans, o.saKey), "m1")
	if countersOf(before, limits.KindBudget) != 1 || countersOf(before, limits.KindRule) != 1 {
		t.Fatalf("before: %d budget and %d rule counters, want 1 and 1", countersOf(before, limits.KindBudget), countersOf(before, limits.KindRule))
	}

	if err := engine.Delete(plans.Engine, engine.Ref{Kind: plan.KindBudget, ID: b.Meta.ID}); err != nil {
		t.Fatal(err)
	}
	cp := keyCaller(t, plans, o.saKey)
	after := mustRoute(t, cp, "m1")
	if countersOf(after, limits.KindBudget) != 0 || countersOf(after, limits.KindRule) != 1 {
		t.Fatalf("after: %d budget and %d rule counters, want 0 and 1", countersOf(after, limits.KindBudget), countersOf(after, limits.KindRule))
	}
	wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungLane, pol.Meta.ID)
	if !hasIssue(plans.Engine.View(), engine.Ref{Kind: plan.KindPolicy, ID: pol.Meta.ID}, plan.IssueRefMissing) {
		t.Error("the policy linking the deleted budget has no ref_missing issue")
	}
}

// Deleting a rate limit a policy links drops only that cap, and never refuses the policy's requests.
func TestDeletedRateLimit_PolicyLosesOnlyThatCap(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	b := o.w.Budget("10")
	rl := o.w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 10, time.Minute))
	pol := o.w.Policy("capped", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID}}, RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	o.w.BindPolicy(pol, o.saAcct())
	plans := o.w.Load(t)

	if err := engine.Delete(plans.Engine, engine.Ref{Kind: plan.KindRateLimit, ID: rl.Meta.ID}); err != nil {
		t.Fatal(err)
	}
	after := mustRoute(t, keyCaller(t, plans, o.saKey), "m1")
	if countersOf(after, limits.KindRule) != 0 || countersOf(after, limits.KindBudget) != 1 {
		t.Fatalf("after: %d rule and %d budget counters, want 0 and 1", countersOf(after, limits.KindRule), countersOf(after, limits.KindBudget))
	}
	if !hasIssue(plans.Engine.View(), engine.Ref{Kind: plan.KindPolicy, ID: pol.Meta.ID}, plan.IssueRefMissing) {
		t.Error("the policy linking the deleted rate limit has no ref_missing issue")
	}
}

// poolLinkWorld is a service account whose policy links one pool, beside credentials its project and the system own that the policy does not link.
func poolLinkWorld() (*org, *policy.Policy, string) {
	o := newOrg()
	linked := o.w.Credential(o.host, nil, plantest.System, "sk-linked")
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	p := o.w.Pool(o.host, linked)
	pol := o.w.Policy("pooled", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	o.w.BindPolicy(pol, o.saAcct())
	return o, pol, p.Meta.ID
}

// Deleting the only pool a policy links narrows the principal to nothing; it never widens to the credentials its scopes own.
func TestDeletedPool_PolicyNarrowsNeverWidens(t *testing.T) {
	o, pol, poolID := poolLinkWorld()
	plans := o.w.Load(t)
	mustRoute(t, keyCaller(t, plans, o.saKey), "m1")

	if err := engine.Delete(plans.Engine, engine.Ref{Kind: plan.KindPool, ID: poolID}); err != nil {
		t.Fatal(err)
	}
	if r := keyCaller(t, plans, o.saKey).Routes.Get("m1"); r.Refused == nil {
		t.Fatalf("after the pool delete the route reaches %v, want a refusal", routeCredentials(r))
	}
	if !hasIssue(plans.Engine.View(), engine.Ref{Kind: plan.KindPolicy, ID: pol.Meta.ID}, plan.IssueRefMissing) {
		t.Error("the policy linking the deleted pool has no ref_missing issue")
	}
}

// A policy whose pool is gone from the store when the plans load (a restart, a resync) narrows the same way.
func TestDeletedPool_NarrowsAfterReload(t *testing.T) {
	o, _, poolID := poolLinkWorld()
	kept := o.w.Pools[:0]
	for _, p := range o.w.Pools {
		if p.Meta.ID != poolID {
			kept = append(kept, p)
		}
	}
	o.w.Pools = kept
	if r := keyCaller(t, o.w.Load(t), o.saKey).Routes.Get("m1"); r.Refused == nil {
		t.Fatalf("after a reload without the pool the route reaches %v, want a refusal", routeCredentials(r))
	}
}

// Deleting a credential a pool holds narrows the pool; the policy keeps the pool's other credentials.
func TestDeletedCredential_PoolNarrows(t *testing.T) {
	o := newOrg()
	a := o.w.Credential(o.host, nil, plantest.System, "sk-a")
	b := o.w.Credential(o.host, nil, plantest.System, "sk-b")
	p := o.w.Pool(o.host, a, b)
	o.w.BindPolicy(o.w.Policy("pooled", policy.Spec{PoolIDs: []string{p.Meta.ID}}), o.saAcct())
	plans := o.w.Load(t)

	if err := engine.Delete(plans.Engine, engine.Ref{Kind: plan.KindCredential, ID: a.Meta.ID}); err != nil {
		t.Fatal(err)
	}
	if got := routeCredentials(mustRoute(t, keyCaller(t, plans, o.saKey), "m1")); !sameList(got, []string{b.Meta.Name}) {
		t.Fatalf("route reaches %v, want only %s", got, b.Meta.Name)
	}
}

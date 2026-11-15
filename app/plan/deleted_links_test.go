package plan_test

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
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

func deleteRow(t *testing.T, plans *plantest.Plans, kind engine.Kind, id string) {
	t.Helper()
	if err := engine.Delete(plans.Engine, engine.Ref{Kind: kind, ID: id}); err != nil {
		t.Fatal(err)
	}
}

// wantRefMissing checks that the row's only ref_missing issue sits at field.
func wantRefMissing(t *testing.T, plans *plantest.Plans, kind engine.Kind, id, field string) {
	t.Helper()
	if got := issueFields(plans.Engine.View(), engine.Ref{Kind: kind, ID: id}, plan.IssueRefMissing); !slices.Equal(got, []string{field}) {
		t.Errorf("%s ref_missing issues at %v, want one at %s", kind, got, field)
	}
}

// Deleting a budget or a rate limit a policy links drops only that cap: the policy keeps its models and its other cap, as if it never linked the deleted one, and says which link is gone.
func TestDeletedCap_PolicyLosesOnlyThatCap(t *testing.T) {
	for _, tc := range []struct {
		name           string
		kind           engine.Kind
		budgets, rules int
		field          string
	}{
		{"budget", plan.KindBudget, 0, 1, "spec.budgets[0]"},
		{"rate limit", plan.KindRateLimit, 1, 0, "spec.rateLimits[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrg()
			o.w.Credential(o.host, nil, plantest.System, "sk-system")
			ids := map[engine.Kind]string{
				plan.KindBudget:    o.w.Budget("10").Meta.ID,
				plan.KindRateLimit: o.w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 10, time.Minute)).Meta.ID,
			}
			pol := o.w.Policy("capped", policy.Spec{Models: []string{"prov/m1"}, Budgets: []policy.BudgetLink{{BudgetID: ids[plan.KindBudget]}}, RateLimits: []policy.RateLimitLink{{RateLimitID: ids[plan.KindRateLimit]}}})
			o.w.BindPolicy(pol, o.saAcct())
			plans := o.w.Load(t)
			if r := mustRoute(t, keyCaller(t, plans, o.saKey), "m1"); countersOf(r, limits.KindBudget) != 1 || countersOf(r, limits.KindRule) != 1 {
				t.Fatalf("before: %d budget and %d rule counters, want 1 and 1", countersOf(r, limits.KindBudget), countersOf(r, limits.KindRule))
			}

			deleteRow(t, plans, tc.kind, ids[tc.kind])

			cp := keyCaller(t, plans, o.saKey)
			if r := mustRoute(t, cp, "m1"); countersOf(r, limits.KindBudget) != tc.budgets || countersOf(r, limits.KindRule) != tc.rules {
				t.Fatalf("after: %d budget and %d rule counters, want %d and %d", countersOf(r, limits.KindBudget), countersOf(r, limits.KindRule), tc.budgets, tc.rules)
			}
			wantRefusal(t, cp.Routes.Get("m2"), plan.CodeModelNotAllowed, limits.RungLane, pol.Meta.ID)
			wantRefMissing(t, plans, plan.KindPolicy, pol.Meta.ID, tc.field)
		})
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

// Losing the only pool a policy links narrows the principal to nothing, never widens it to the credentials its scopes own: by a delete, and the same when the plans load without the pool (a restart, a resync).
func TestDeletedPool_PolicyNarrowsNeverWidens(t *testing.T) {
	for _, reload := range []bool{false, true} {
		o, pol, poolID := poolLinkWorld()
		if reload {
			o.w.Pools = slices.DeleteFunc(o.w.Pools, func(p *pool.Pool) bool { return p.Meta.ID == poolID })
		}
		plans := o.w.Load(t)
		if !reload {
			mustRoute(t, keyCaller(t, plans, o.saKey), "m1")
			deleteRow(t, plans, plan.KindPool, poolID)
		}
		wantRefusal(t, keyCaller(t, plans, o.saKey).Routes.Get("m1"), plan.CodeNoCredentials, limits.RungLane, pol.Meta.ID)
		wantRefMissing(t, plans, plan.KindPolicy, pol.Meta.ID, "spec.poolIds[0]")
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

	deleteRow(t, plans, plan.KindCredential, a.Meta.ID)

	if got := routeCredentials(mustRoute(t, keyCaller(t, plans, o.saKey), "m1")); !slices.Equal(got, []string{b.Meta.Name}) {
		t.Fatalf("route reaches %v, want only %s", got, b.Meta.Name)
	}
	wantRefMissing(t, plans, plan.KindPool, p.Meta.ID, "spec.credentialIds[0]")
}

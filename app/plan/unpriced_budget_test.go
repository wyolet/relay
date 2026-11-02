package plan_test

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
)

// unpricedOrg adds a model "free" served at the org's host with no pricing.
func unpricedOrg() *org {
	o := newOrg()
	m := o.w.Model(o.prov, "free")
	o.w.Bind(m, o.host, adapters.OpenAI)
	return o
}

// A model without pricing is refused under a caller budget, naming the rung and policy, so a budget has no hole; priced models still route.
func TestUnpriced_RefusedUnderACallerBudget(t *testing.T) {
	o := unpricedOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	pol := o.w.Policy("budgeted", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: o.w.Budget("5").Meta.ID}}})
	o.w.BindPolicy(pol, o.projectAcct())
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	wantRefusal(t, cp.Routes.Get("free"), plan.CodeUnpricedUnderBudget, limits.RungProject, pol.Meta.ID)
	mustRoute(t, cp, "m1")
}

// The same model routes when no budget applies anywhere on the ladder.
func TestUnpriced_RoutesWithoutABudget(t *testing.T) {
	o := unpricedOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	mustRoute(t, keyCaller(t, o.w.Load(t), o.saKey), "free")
}

// An upstream budget (on the account) counts too: an unpriced model is refused on that account's credentials.
func TestUnpriced_RefusedUnderAnAccountBudget(t *testing.T) {
	o := unpricedOrg()
	acct := o.w.Account(o.host)
	o.w.Credential(o.host, acct, plantest.System, "sk-acct")
	tier := o.w.Policy("tier", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: o.w.Budget("100").Meta.ID}}})
	o.w.BindPolicy(tier, acct)
	cp := keyCaller(t, o.w.Load(t), o.saKey)

	r := cp.Routes.Get("free")
	if r.Refused == nil || r.Refused.Code != plan.CodeUnpricedUnderBudget {
		t.Fatalf("route = %v (refusal %+v), want unpriced_under_budget", routeCredentials(r), r.Refused)
	}
	mustRoute(t, cp, "m1")
}

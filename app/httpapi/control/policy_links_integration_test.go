//go:build integration

package control

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/budget"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
)

// A policy saved in one project may not link another project's pool or budget: the plans follow the link with no check of their own, so the save is the only gate.
func TestPolicySaveRefusesAnotherProjectsPoolAndBudget(t *testing.T) {
	db, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: db})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	ctx = visibleCtx(ctx)

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "links-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	projA, projB := meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}, meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
	privatePool := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "a-private-pool", Owner: projA}, Spec: pool.Spec{HostID: hst.Meta.ID}}
	if err := stores.Pool.Upsert(ctx, privatePool); err != nil {
		t.Fatalf("upsert pool: %v", err)
	}
	privateBudget := &budget.Budget{Meta: meta.Metadata{ID: meta.NewID(), Name: "a-private-budget", Owner: projA}, Spec: budget.Spec{AmountUSD: "10", Period: budget.PeriodMonth, Mode: budget.ModeHard}}
	if err := stores.Budget.Upsert(ctx, privateBudget); err != nil {
		t.Fatalf("upsert budget: %v", err)
	}

	for name, spec := range map[string]policy.Spec{
		"pool":   {PoolIDs: []string{privatePool.Meta.ID}},
		"budget": {Budgets: []policy.BudgetLink{{BudgetID: privateBudget.Meta.ID}}},
	} {
		inB := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "b-policy", Owner: projB}, Spec: spec}
		err := refErr(refs(d).Policy(ctx, inB))
		if statusOf(t, err) != 400 || !strings.Contains(err.Error(), "another scope") {
			t.Errorf("project B linking project A's %s: err = %v, want a 400 naming the other scope", name, err)
		}
		inA := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "a-policy", Owner: projA}, Spec: spec}
		if err := refErr(refs(d).Policy(ctx, inA)); err != nil {
			t.Errorf("project A linking its own %s: %v", name, err)
		}
	}
}

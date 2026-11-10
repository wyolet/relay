package apply

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/policy"
	apppool "github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// linkedRows holds a system policy linking a pool, a rate limit and a
// budget, each user-owned and labelled for the prune.
func linkedRows() (*Rows, *policy.Policy) {
	owner := meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
	row := func(name string) meta.Metadata {
		return meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner, Labels: pruneLabels}
	}
	pl := &apppool.Pool{Meta: row("pool")}
	rl := &ratelimit.RateLimit{Meta: row("limit")}
	bd := &budget.Budget{Meta: row("cap")}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "team-gate", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{
			PoolIDs:    []string{pl.Meta.ID},
			RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}, Budgets: []policy.BudgetLink{{BudgetID: bd.Meta.ID}},
		},
	}
	return &Rows{
		Pools: []*apppool.Pool{pl}, RateLimits: []*ratelimit.RateLimit{rl},
		Budgets: []*budget.Budget{bd}, Policies: []*policy.Policy{pol},
	}, pol
}

// Pruning a row a policy links is allowed, and the plan names the policy and
// the field that loses the link, like the API's delete answer.
func TestPruneReportsThePoliciesThatLoseALink(t *testing.T) {
	rows, _ := linkedRows()
	b := pruneAll(rows)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := map[string]string{"Pool": "spec.pools", "RateLimit": "spec.rateLimits", "Budget": "spec.budgets"}
	for _, e := range b.entries {
		field, ok := want[e.Kind]
		if !ok || e.Action != ActionDelete {
			continue
		}
		delete(want, e.Kind)
		if e.Detached == nil || e.Detached.Field != field || !reflect.DeepEqual(e.Detached.Policies, []string{"team-gate"}) {
			t.Errorf("%s delete reported %+v, want team-gate losing %s", e.Kind, e.Detached, field)
		}
	}
	if len(want) != 0 {
		t.Fatalf("no delete planned for %v", want)
	}
}

// The policies a prune takes links from are kept on the entry, in the field
// vocabulary the control API's delete audits them with.
func TestPruneKeepsThePoliciesToAudit(t *testing.T) {
	rows, pol := linkedRows()
	b := pruneAll(rows)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := map[string]string{"Pool": "spec.poolIds", "RateLimit": "spec.rateLimits[].rateLimitId", "Budget": "spec.budgets[].budgetId"}
	for _, e := range b.entries {
		field, ok := want[e.Kind]
		if !ok || e.Action != ActionDelete {
			continue
		}
		delete(want, e.Kind)
		got := e.DetachedPolicies()
		if len(got) != 1 || got[0] != (DetachedPolicy{ID: pol.Meta.ID, Name: pol.Meta.Name, Owner: pol.Meta.Owner, Field: field}) {
			t.Errorf("%s delete keeps %+v, want %s losing %s", e.Kind, got, pol.Meta.Name, field)
		}
	}
	if len(want) != 0 {
		t.Fatalf("no delete planned for %v", want)
	}
}

// A policy the same bundle rewrites without the link loses nothing to the
// prune, so nothing is reported.
func TestPruneReportsAgainstThePolicyTheBundleDeclares(t *testing.T) {
	rows, pol := linkedRows()
	b := pruneAll(rows)
	doc := "kind: Policy\nmetadata: {name: " + pol.Meta.Name + ", owner: {kind: system}}\nspec: {}\n"
	if err := b.run(context.Background(), userDocs(t, docHeader+doc)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, e := range b.entries {
		if e.Action == ActionDelete && e.Detached != nil {
			t.Errorf("%s %s reported %+v though the declared policy no longer links it", e.Kind, e.Name, e.Detached)
		}
	}
}

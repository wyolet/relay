package apply

import (
	"context"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/app/budget"
	apppool "github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
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

// Pruning a row a policy links is allowed. The plan names the policy and the
// field that loses the link, like the API's delete answer, and the entry
// keeps the policy in the field vocabulary the delete is audited with.
func TestPruneReportsThePoliciesThatLoseALink(t *testing.T) {
	rows, pol := linkedRows()
	b := pruneAll(rows)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("plan: %v", err)
	}
	type report struct {
		Kind     string
		Detached DetachedLinks
		Audit    []DetachedPolicy
	}
	audit := func(field string) []DetachedPolicy {
		return []DetachedPolicy{{ID: pol.Meta.ID, Name: "team-gate", Owner: pol.Meta.Owner, Field: field}}
	}
	want := []report{
		{"Pool", DetachedLinks{Field: "spec.pools", Policies: []string{"team-gate"}}, audit("spec.poolIds")},
		{"Budget", DetachedLinks{Field: "spec.budgets", Policies: []string{"team-gate"}}, audit("spec.budgets[].budgetId")},
		{"RateLimit", DetachedLinks{Field: "spec.rateLimits", Policies: []string{"team-gate"}}, audit("spec.rateLimits[].rateLimitId")},
	}
	var got []report
	for _, e := range b.entries {
		if e.Action != ActionDelete || e.Detached == nil {
			t.Fatalf("entry %s %q = %s with %+v, want a delete reporting its links", e.Kind, e.Name, e.Action, e.Detached)
		}
		got = append(got, report{e.Kind, *e.Detached, e.DetachedPolicies()})
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("deletes (-want +got):\n%s", diff)
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
	var deletes []string
	for _, e := range b.entries {
		if e.Action != ActionDelete {
			continue
		}
		deletes = append(deletes, e.Kind)
		if e.Detached != nil || e.DetachedPolicies() != nil {
			t.Errorf("%s %s reported %+v though the declared policy no longer links it", e.Kind, e.Name, e.Detached)
		}
	}
	if want := []string{"Pool", "Budget", "RateLimit"}; !slices.Equal(deletes, want) {
		t.Fatalf("deletes = %v, want %v", deletes, want)
	}
}

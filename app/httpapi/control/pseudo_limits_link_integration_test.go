//go:build integration

package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// spec.link is stored as the new link's own fields in every case, answered as stored, and the plans built from the result count a per-lane link per lane and hold a rate limit's model scope. Without link the link counts once, over every model.
func TestPseudoLimits_LinkOptions(t *testing.T) {
	for _, opt := range []struct {
		name  string
		limit map[string]any
		want  PseudoLimitLink
	}{
		{"budget, no link", aBudget, PseudoLimitLink{Shared: true}},
		{"budget per lane", withLink(aBudget, map[string]any{"shared": false}), PseudoLimitLink{Shared: false}},
		{"rate limit per lane", withLink(aRateLimit, map[string]any{"shared": false}), PseudoLimitLink{Shared: false}},
		{"rate limit on a model", withLink(aRateLimit, map[string]any{"models": []string{"Acme/M1"}}), PseudoLimitLink{Shared: true, Models: []string{"acme/m1"}}},
	} {
		for _, c := range []pseudo.PolicyCase{pseudo.CaseNone, pseudo.CaseOwn, pseudo.CaseCopy} {
			t.Run(opt.name+"/"+string(c), func(t *testing.T) {
				w := newPolicyStepWorld(t)
				kept := policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}}
				switch c {
				case pseudo.CaseOwn:
					w.bind(t, w.policy(t, "two-app-own", w.owner, kept), w.owner, w.projTwoOwn)
				case pseudo.CaseCopy:
					w.bind(t, w.policy(t, "two-shared", w.two, kept), w.two, w.teamTwoOwn, w.projTwoOwn)
				}
				w.loadPlans(t)
				rec, out := w.limits(t, "bob", target("project", w.projTwo.Meta.ID), opt.limit, false)
				wantStatus(t, rec, http.StatusCreated)
				made := out.Budget
				if made == nil {
					made = out.RateLimit
				}
				if out.Case != c || made == nil {
					t.Fatalf("answer = %+v", out)
				}
				if diff := cmp.Diff(opt.want, made.Link); diff != "" {
					t.Errorf("answered link (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(opt.want, storedLink(w.heldPolicy(t, w.projTwoOwn.Meta.ID), made.ID), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("stored link (-want +got):\n%s", diff)
				}
				w.loadPlans(t)
				if diff := cmp.Diff(planLink{PerLane: !opt.want.Shared, Scoped: len(opt.want.Models) > 0, Models: opt.want.Models}, w.plannedLink(t, made.ID), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("planned link (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// A model scope the policy update would refuse is refused at /spec/link/models, as is any model scope on a budget; nothing is written.
func TestPseudoLimits_LinkModelsRefused(t *testing.T) {
	w := newPolicyStepWorld(t)
	w.loadPlans(t)
	before := w.rowCounts(t)
	for name, limit := range map[string]map[string]any{
		"a host":             withLink(aRateLimit, map[string]any{"models": []string{"@share-host"}}),
		"no enabled binding": withLink(aRateLimit, map[string]any{"models": []string{"acme/m9"}}),
		"a budget":           withLink(aBudget, map[string]any{"models": []string{"acme"}}),
	} {
		rec, _ := w.limits(t, "bob", target("project", w.projTwo.Meta.ID), limit, true)
		if r := refusalOf(t, rec); rec.Code != http.StatusBadRequest || r.Pointer != "/spec/link/models" {
			t.Errorf("%s: %d %s, want 400 at /spec/link/models", name, rec.Code, rec.Body)
		}
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("rows written (-before +after):\n%s", diff)
	}
}

func withLink(limit map[string]any, link map[string]any) map[string]any {
	out := map[string]any{"link": link}
	for k, v := range limit {
		out[k] = v
	}
	return out
}

// storedLink is p's link to the budget or rate limit id, as the answer shows a link.
func storedLink(p *policy.Policy, id string) PseudoLimitLink {
	for _, l := range p.Spec.RateLimits {
		if l.RateLimitID == id {
			return PseudoLimitLink{Shared: l.IsShared(), Models: l.Models}
		}
	}
	for _, l := range p.Spec.Budgets {
		if l.BudgetID == id {
			return PseudoLimitLink{Shared: l.IsShared()}
		}
	}
	return PseudoLimitLink{}
}

// planLink is what the plans make of one link on the project's rung: counted per lane, and scoped to models.
type planLink struct {
	PerLane bool
	Scoped  bool
	Models  []string
}

// plannedLink reads the link to the limit id on project two's own rung in the current plans.
func (w *policyStepWorld) plannedLink(t *testing.T, id string) planLink {
	t.Helper()
	for _, rung := range plan.ScopeLimits(w.plans.View(), w.projTwoOwn) {
		if rung.Rung.Account == nil || rung.Rung.Account.Meta.ID != w.projTwoOwn.Meta.ID {
			continue
		}
		for _, rl := range rung.RateLimits {
			if rl.Link.RateLimit.Meta.ID == id {
				return planLink{PerLane: rl.PerLane, Scoped: rl.Link.Scoped, Models: rl.Link.Models}
			}
		}
		for _, b := range rung.Budgets {
			if b.Link.Budget.Meta.ID == id {
				return planLink{PerLane: b.PerLane}
			}
		}
	}
	t.Fatalf("project two's rung links no limit %s", id)
	return planLink{}
}

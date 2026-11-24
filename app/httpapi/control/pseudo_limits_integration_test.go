//go:build integration

package control

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

var (
	aBudget    = map[string]any{"budget": map[string]any{"amountUsd": "100"}}
	aRateLimit = map[string]any{"rateLimit": map[string]any{"rules": []map[string]any{{"meter": "requests", "amount": 10, "window": 60, "strategy": "sliding-window"}}}}
)

// A budget or a rate limit is made in the target's scope and linked beside what the policy links already, in each case: none makes a policy of the link alone, own adds it in place, copy carries the links over and adds it to the copy.
func TestPseudoLimits_Cases(t *testing.T) {
	for _, limit := range []string{"budget", "rateLimit"} {
		for _, c := range []pseudo.PolicyCase{pseudo.CaseNone, pseudo.CaseOwn, pseudo.CaseCopy} {
			t.Run(limit+"/"+string(c), func(t *testing.T) {
				w := newPolicyStepWorld(t)
				rl := w.rateLimitFor(t, w.owner)
				kept := policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}, RateLimits: []policy.RateLimitLink{{RateLimitID: rl}}}
				switch c {
				case pseudo.CaseOwn:
					w.bind(t, w.policy(t, "two-app-own", w.owner, kept), w.owner, w.projTwoOwn)
				case pseudo.CaseCopy:
					w.bind(t, w.policy(t, "two-shared", w.two, kept), w.two, w.teamTwoOwn, w.projTwoOwn)
				}
				w.loadPlans(t)
				body := aBudget
				if limit == "rateLimit" {
					body = aRateLimit
				}
				rec, out := w.limits(t, "bob", target("project", w.projTwo.Meta.ID), body, false)
				wantStatus(t, rec, http.StatusCreated)
				made := out.Budget
				if limit == "rateLimit" {
					made = out.RateLimit
				}
				if out.Case != c || made == nil || out.Policy == nil {
					t.Fatalf("answer = %+v", out)
				}
				got := w.heldPolicy(t, w.projTwoOwn.Meta.ID)
				if got == nil || got.Meta.ID != out.Policy.ID || got.Meta.Owner != w.owner {
					t.Fatalf("account held under %+v", got)
				}
				var linked []string
				for _, l := range got.Spec.RateLimits {
					linked = append(linked, l.RateLimitID)
				}
				linked = append(linked, got.BudgetIDs()...)
				want := []string{made.ID}
				if c != pseudo.CaseNone {
					want = []string{rl, made.ID}
					if !slices.Equal(got.Spec.PoolIDs, kept.PoolIDs) {
						t.Errorf("pools = %v, want %v kept", got.Spec.PoolIDs, kept.PoolIDs)
					}
				}
				slices.Sort(want)
				slices.Sort(linked)
				if diff := cmp.Diff(want, linked); diff != "" {
					t.Errorf("links (-want +got):\n%s", diff)
				}
				if limit == "budget" {
					b, err := w.stores.Budget.Get(t.Context(), made.ID)
					must(t, "budget", err)
					if b == nil || b.Meta.Owner != w.owner || b.Meta.Name != "ci-cap" {
						t.Errorf("budget = %+v", b)
					}
				} else {
					r, err := w.stores.RateLimit.Get(t.Context(), made.ID)
					must(t, "rate limit", err)
					if r == nil || r.Meta.Owner != w.owner {
						t.Errorf("rate limit = %+v", r)
					}
				}
			})
		}
	}
}

// A budget answers the models the target uses now that have no pricing, in the dry run and the write, and goes through.
func TestPseudoLimits_UnpricedModels(t *testing.T) {
	w := newPolicyStepWorld(t)
	w.bind(t, w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}}), w.owner, w.projTwoOwn)
	w.loadPlans(t)
	tgt := target("project", w.projTwo.Meta.ID)
	for _, dry := range []bool{true, false} {
		rec, out := w.limits(t, "bob", tgt, aBudget, dry)
		if rec.Code >= 300 {
			t.Fatalf("dry run %v: %d %s", dry, rec.Code, rec.Body)
		}
		if diff := cmp.Diff([]string{"acme/m2"}, out.UnpricedModels); diff != "" {
			t.Errorf("dry run %v: unpriced models (-want +got):\n%s", dry, diff)
		}
	}
	rec, out := w.limits(t, "bob", tgt, aRateLimit, true)
	wantStatus(t, rec, http.StatusOK)
	if out.UnpricedModels != nil {
		t.Errorf("a rate limit answered unpriced models %v", out.UnpricedModels)
	}
}

// above: the account is held by a binding or policy the caller may not see or change. Each variant answers 403 policy_set_above naming the scope (by name only where the caller sees it), says nothing of the hidden rows, writes nothing, and the prefill answers the same.
func TestPseudoStep_Above(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, w *policyStepWorld) (secrets []string, scope string)
	}{
		{"a binding of the organization", func(t *testing.T, w *policyStepWorld) ([]string, string) {
			p := w.policy(t, "org-secret-policy", meta.Global, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}})
			b := w.bind(t, p, meta.Global, w.projTwoOwn)
			return []string{p.Meta.ID, p.Meta.Name, b.Meta.ID, b.Meta.Name}, "the organization"
		}},
		{"a binding the caller may not see", func(t *testing.T, w *policyStepWorld) ([]string, string) {
			p := w.policy(t, "two-secret-policy", w.two, policy.Spec{})
			b := w.bind(t, p, w.two, w.projTwoOwn)
			w.hidden[b.Meta.ID] = true
			return []string{p.Meta.ID, p.Meta.Name, b.Meta.ID, b.Meta.Name}, `team "two"`
		}},
		{"a policy the caller may not see, of a scope it sees", func(t *testing.T, w *policyStepWorld) ([]string, string) {
			p := w.policy(t, "one-secret-policy", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}, policy.Spec{})
			w.hidden[p.Meta.ID] = true
			w.bind(t, p, w.owner, w.projTwoOwn)
			return []string{p.Meta.ID, p.Meta.Name}, `team "one"`
		}},
		{"a policy of a user the caller may not see", func(t *testing.T, w *policyStepWorld) ([]string, string) {
			p := w.policy(t, "carol-secret-policy", meta.Owner{Kind: meta.OwnerUser, ID: w.users["carol"]}, policy.Spec{})
			w.hidden[p.Meta.ID] = true
			w.bind(t, p, w.owner, w.projTwoOwn)
			return []string{p.Meta.ID, p.Meta.Name, "carol"}, "a user you may not see"
		}},
		{"a binding the caller may not change", func(t *testing.T, w *policyStepWorld) ([]string, string) {
			p := w.policy(t, "two-shared", w.two, policy.Spec{})
			w.bind(t, p, w.two, w.projTwoOwn, w.teamTwoOwn)
			*w.denied = "policy-bindings.update"
			return nil, `team "two"`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPolicyStepWorld(t)
			secrets, scope := tc.setup(t, w)
			defer func() { *w.denied = "" }()
			w.loadPlans(t)
			before := w.rowCounts(t)
			tgt := target("project", w.projTwo.Meta.ID)
			for name, send := range map[string]func() *httptest.ResponseRecorder{
				"access": func() *httptest.ResponseRecorder {
					rec, _ := w.access(t, "bob", tgt, map[string]any{"everyCredential": true, "models": []string{"acme"}}, false)
					return rec
				},
				"limits":  func() *httptest.ResponseRecorder { rec, _ := w.limits(t, "bob", tgt, aBudget, false); return rec },
				"prefill": func() *httptest.ResponseRecorder { rec, _ := w.access(t, "bob", tgt, nil, true); return rec },
			} {
				rec := send()
				body := rec.Body.String()
				if r := refusalOf(t, rec); rec.Code != http.StatusForbidden || r.Err.Code != codePolicySetAbove || r.Pointer != "/spec/target" || !strings.Contains(r.Err.Message, scope) {
					t.Errorf("%s: %d %s, want 403 policy_set_above at /spec/target naming %s", name, rec.Code, body, scope)
				}
				for _, s := range secrets {
					if strings.Contains(body, s) {
						t.Errorf("%s: the answer says %q of a hidden row: %s", name, s, body)
					}
				}
			}
			if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
				t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
			}
		})
	}
}

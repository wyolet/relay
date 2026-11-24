package pseudo

import (
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// The limit is made in the target's scope first, then linked beside what the policy links already, in every case.
func TestExpandLimits(t *testing.T) {
	rule := ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: "sliding-window"}
	kept := policy.Spec{Models: []string{"acme"}, PoolIDs: []string{"pool"}, RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}}, Budgets: []policy.BudgetLink{{BudgetID: "bu"}}}
	for _, limit := range []string{"budget", "rate-limit"} {
		for _, c := range []PolicyCase{CaseNone, CaseOwn, CaseCopy} {
			t.Run(limit+"/"+string(c), func(t *testing.T) {
				w := newAccessWorld()
				in := w.reads().Policy
				in.Policy = &policy.Policy{Meta: meta.Metadata{ID: "pol", Name: "pol", Owner: proj}, Spec: cloneSpec(kept)}
				switch c {
				case CaseNone:
					in.Held, in.Policy = nil, nil
				case CaseCopy:
					in.Alone = false
				}
				body := Limits{Metadata: LimitsMetadata{DisplayName: "CI cap"}, Spec: LimitsSpec{Target: Target{Kind: "project", ID: targetID}}}
				if limit == "budget" {
					body.Spec.Budget = &budget.Spec{AmountUSD: "100"}
				} else {
					body.Spec.RateLimit = &ratelimit.Spec{Rules: []ratelimit.Rule{rule}}
				}
				got, err := ExpandLimits(body, in)
				if err != nil {
					t.Fatal(err)
				}
				m := meta.Metadata{ID: "id-1", Name: "ci-cap", DisplayName: "CI cap", Owner: proj}
				want := policy.Spec{}
				if c != CaseNone {
					want = cloneSpec(kept)
				}
				var row Row
				if limit == "budget" {
					b := &budget.Budget{Meta: m, Spec: *body.Spec.Budget}
					row = Row{Kind: "budget", Pointer: "/spec/budget", Meta: &b.Meta, Value: b}
					want.Budgets = append(want.Budgets, policy.BudgetLink{BudgetID: "id-1"})
				} else {
					rl := &ratelimit.RateLimit{Meta: m, Spec: *body.Spec.RateLimit}
					row = Row{Kind: "rate-limit", Pointer: "/spec/rateLimit", Meta: &rl.Meta, Value: rl}
					want.RateLimits = append(want.RateLimits, policy.RateLimitLink{RateLimitID: "id-1"})
				}
				if got.Case != c || len(got.Rows) == 0 {
					t.Fatalf("case %s, rows %d", got.Case, len(got.Rows))
				}
				if diff := cmp.Diff(row, got.Rows[0]); diff != "" {
					t.Errorf("limit row (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(want, got.Policy.Spec); diff != "" {
					t.Errorf("policy spec (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// spec.link sets the new link's shared flag, and a rate limit's models, in every case; the links the policy held stay as they were.
func TestExpandLimits_Link(t *testing.T) {
	no := false
	rule := ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: "sliding-window"}
	held := policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}}, Budgets: []policy.BudgetLink{{BudgetID: "bu"}}}
	for _, tc := range []struct {
		name string
		spec LimitsSpec
		want policy.Spec
	}{
		{"budget per lane", LimitsSpec{Budget: &budget.Spec{AmountUSD: "1"}, Link: &LimitLink{Shared: &no}},
			policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: "id-1", Shared: &no}}}},
		{"rate limit per lane", LimitsSpec{RateLimit: &ratelimit.Spec{Rules: []ratelimit.Rule{rule}}, Link: &LimitLink{Shared: &no}},
			policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: "id-1", Shared: &no}}}},
		{"rate limit on models", LimitsSpec{RateLimit: &ratelimit.Spec{Rules: []ratelimit.Rule{rule}}, Link: &LimitLink{Models: []string{"acme/m1"}}},
			policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: "id-1", Models: []string{"acme/m1"}}}}},
	} {
		for _, c := range []PolicyCase{CaseNone, CaseOwn, CaseCopy} {
			t.Run(tc.name+"/"+string(c), func(t *testing.T) {
				in := newAccessWorld().reads().Policy
				in.Policy = &policy.Policy{Meta: meta.Metadata{ID: "pol", Name: "pol", Owner: proj}, Spec: cloneSpec(held)}
				switch c {
				case CaseNone:
					in.Held, in.Policy = nil, nil
				case CaseCopy:
					in.Alone = false
				}
				tc.spec.Target = Target{Kind: "project", ID: targetID}
				got, err := ExpandLimits(Limits{Metadata: LimitsMetadata{DisplayName: "CI cap"}, Spec: tc.spec}, in)
				if err != nil {
					t.Fatal(err)
				}
				want := tc.want
				if c != CaseNone {
					want.RateLimits = append(slices.Clone(held.RateLimits), want.RateLimits...)
					want.Budgets = append(slices.Clone(held.Budgets), want.Budgets...)
				}
				if got.Case != c {
					t.Fatalf("case %s", got.Case)
				}
				if diff := cmp.Diff(want, got.Policy.Spec, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("policy spec (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// A budget link has no model scope: models with a budget is refused before anything is read.
func TestCheckLimits_BudgetModels(t *testing.T) {
	body := Limits{Spec: LimitsSpec{Target: Target{Kind: "project", ID: targetID}, Budget: &budget.Spec{AmountUSD: "1"}, Link: &LimitLink{Models: []string{"acme"}}}}
	want := &BodyError{Pointer: "/spec/link/models", Message: "spec.link.models applies to a rate limit only: a budget link counts spend on every model"}
	if diff := cmp.Diff(want, CheckLimits(body)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestExpandLimits_ExactlyOne(t *testing.T) {
	in := newAccessWorld().reads().Policy
	target := Target{Kind: "team", ID: targetID}
	for _, spec := range []LimitsSpec{{Target: target}, {Target: target, Budget: &budget.Spec{AmountUSD: "1"}, RateLimit: &ratelimit.Spec{}}} {
		_, err := ExpandLimits(Limits{Spec: spec}, in)
		if diff := cmp.Diff(&BodyError{Pointer: "/spec", Message: "exactly one of spec.budget and spec.rateLimit is required"}, err); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	}
}

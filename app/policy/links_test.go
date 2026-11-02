package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func TestValidateLinks(t *testing.T) {
	rl := meta.NewID()
	each := false
	for _, tc := range []struct {
		name string
		edit func(p *Policy)
		want string // "" = valid
	}{
		{name: "pools, limits and budgets", edit: func(p *Policy) {
			p.Spec.PoolIDs = []string{meta.NewID(), meta.NewID()}
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl}, {RateLimitID: meta.NewID(), Models: []string{"openai/gpt-4o"}}}
			p.Spec.Budgets = []BudgetLink{{BudgetID: meta.NewID()}}
		}},
		{name: "one rate limit over different models", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{
				{RateLimitID: rl, Models: []string{"openai/gpt-4o"}},
				{RateLimitID: rl, Models: []string{"openai/gpt-4o-mini"}},
			}
		}},
		{name: "one rate limit shared and per lane", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl}, {RateLimitID: rl, Shared: &each}}
		}},
		{name: "host tier with limits and budgets", edit: func(p *Policy) {
			p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl, Models: []string{"openai/gpt-4o"}}}
			p.Spec.Budgets = []BudgetLink{{BudgetID: meta.NewID()}}
		}},
		{name: "pool id must be an id", edit: func(p *Policy) { p.Spec.PoolIDs = []string{"anthropic-main"} }, want: "PoolIDs"},
		{name: "budget id must be an id", edit: func(p *Policy) { p.Spec.Budgets = []BudgetLink{{BudgetID: "ml-monthly"}} }, want: "BudgetID"},
		{name: "rate limit id required", edit: func(p *Policy) { p.Spec.RateLimits = []RateLimitLink{{}} }, want: "RateLimitID"},
		{name: "duplicate pool", edit: func(p *Policy) { id := meta.NewID(); p.Spec.PoolIDs = []string{id, id} }, want: "duplicate poolIds"},
		{name: "duplicate budget", edit: func(p *Policy) {
			id := meta.NewID()
			p.Spec.Budgets = []BudgetLink{{BudgetID: id}, {BudgetID: id, Shared: &each}}
		}, want: "duplicate budgets"},
		{name: "one rate limit twice over the same models", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{
				{RateLimitID: rl, Models: []string{"openai/gpt-4o", "openai/o1"}},
				{RateLimitID: rl, Models: []string{"openai/o1", "openai/gpt-4o"}},
			}
		}, want: "over the same models"},
		{name: "one rate limit twice unscoped", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl}, {RateLimitID: rl}}
		}, want: "over the same models"},
		{name: "unparseable model scope", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl, Models: []string{"openai/"}}}
		}, want: "rateLimits[0]"},
		{name: "a model scope naming a host", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl, Models: []string{"openai/gpt-4o@azure"}}}
		}, want: "names a host"},
		{name: "host tier with pools", edit: func(p *Policy) {
			p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}
			p.Spec.PoolIDs = []string{meta.NewID()}
		}, want: "must not list poolIds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fix("p")
			tc.edit(p)
			err := p.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate() = %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

// The read form every boundary returns: models a list, every link's shared flag explicit, omitted read as true.
func TestNormalize(t *testing.T) {
	each := false
	p := fix("p")
	p.Spec.Models = nil
	p.Spec.RateLimits = []RateLimitLink{{RateLimitID: meta.NewID()}, {RateLimitID: meta.NewID(), Shared: &each}}
	p.Spec.Budgets = []BudgetLink{{BudgetID: meta.NewID()}}
	p.Normalize()
	if p.Spec.Models == nil || len(p.Spec.Models) != 0 {
		t.Errorf("models = %#v, want []", p.Spec.Models)
	}
	if l := p.Spec.RateLimits; l[0].Shared == nil || !*l[0].Shared || l[1].Shared == nil || *l[1].Shared {
		t.Errorf("rate-limit shared = %v, %v; want true, false", l[0].Shared, l[1].Shared)
	}
	if b := p.Spec.Budgets[0]; b.Shared == nil || !*b.Shared {
		t.Errorf("budget shared = %v, want true", b.Shared)
	}
}

// A rate-limit scope stored by the control API and the same scope declared in a manifest must compare equal, or apply reports drift on every run.
func TestCanonicalizeSpecModelRefsRewritesRateLimitScopes(t *testing.T) {
	p := fix("p")
	p.Spec.RateLimits = []RateLimitLink{{RateLimitID: meta.NewID(), Models: []string{"OpenAI/GPT-4o", "openai/gpt-4o"}}}
	if err := p.CanonicalizeSpecModelRefs(); err != nil {
		t.Fatal(err)
	}
	if got := p.Spec.RateLimits[0].Models; !slices.Equal(got, []string{"openai/gpt-4o"}) {
		t.Fatalf("models = %v, want [openai/gpt-4o]", got)
	}
}

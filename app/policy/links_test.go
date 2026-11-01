package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func TestValidateLinks(t *testing.T) {
	rl := meta.NewID()
	for _, tc := range []struct {
		name string
		edit func(p *Policy)
		want string // "" = valid
	}{
		{name: "pools, credentials, limits and budgets", edit: func(p *Policy) {
			p.Spec.PoolIDs = []string{meta.NewID(), meta.NewID()}
			p.Spec.CredentialIDs = []string{meta.NewID()}
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl}, {RateLimitID: meta.NewID(), Models: []string{"openai/gpt-4o"}}}
			p.Spec.BudgetIDs = []string{meta.NewID()}
		}},
		{name: "one rate limit over different models", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{
				{RateLimitID: rl, Models: []string{"openai/gpt-4o"}},
				{RateLimitID: rl, Models: []string{"openai/gpt-4o-mini"}},
			}
		}},
		{name: "host tier with limits and budgets", edit: func(p *Policy) {
			p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl, Models: []string{"openai/gpt-4o"}}}
			p.Spec.BudgetIDs = []string{meta.NewID()}
		}},
		{name: "pool id must be an id", edit: func(p *Policy) { p.Spec.PoolIDs = []string{"anthropic-main"} }, want: "PoolIDs"},
		{name: "credential id must be an id", edit: func(p *Policy) { p.Spec.CredentialIDs = []string{"my-key"} }, want: "CredentialIDs"},
		{name: "budget id must be an id", edit: func(p *Policy) { p.Spec.BudgetIDs = []string{"ml-monthly"} }, want: "BudgetIDs"},
		{name: "rate limit id required", edit: func(p *Policy) { p.Spec.RateLimits = []RateLimitLink{{}} }, want: "RateLimitID"},
		{name: "duplicate pool", edit: func(p *Policy) { id := meta.NewID(); p.Spec.PoolIDs = []string{id, id} }, want: "duplicate poolIds"},
		{name: "duplicate credential", edit: func(p *Policy) { id := meta.NewID(); p.Spec.CredentialIDs = []string{id, id} }, want: "duplicate credentialIds"},
		{name: "duplicate budget", edit: func(p *Policy) { id := meta.NewID(); p.Spec.BudgetIDs = []string{id, id} }, want: "duplicate budgetIds"},
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
		{name: "rateLimits beside rateLimitId", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl}}
			p.Spec.RateLimitID = meta.NewID()
		}, want: "rateLimits replaces"},
		{name: "rateLimits beside rlBindings", edit: func(p *Policy) {
			p.Spec.RateLimits = []RateLimitLink{{RateLimitID: rl}}
			p.Spec.RLBindings = []RLBinding{{Models: []string{"openai/gpt-4o"}, RateLimitID: meta.NewID()}}
		}, want: "rateLimits replaces"},
		{name: "credentialIds beside hostKeyIds", edit: func(p *Policy) {
			p.Spec.CredentialIDs = []string{meta.NewID()}
			p.Spec.HostKeyIDs = []string{meta.NewID()}
		}, want: "credentialIds replaces"},
		{name: "host tier with pools", edit: func(p *Policy) {
			p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}
			p.Spec.PoolIDs = []string{meta.NewID()}
		}, want: "must not list poolIds or credentialIds"},
		{name: "host tier with credentials", edit: func(p *Policy) {
			p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}
			p.Spec.CredentialIDs = []string{meta.NewID()}
		}, want: "must not list poolIds or credentialIds"},
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

package policy_test

import (
	"testing"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// A provider wildcard ("anthropic/*") is a valid grant and a valid rate-limit scope, as the Policy shape shows.
func TestValidate_AcceptsProviderWildcard(t *testing.T) {
	p := &policy.Policy{
		Meta: meta.Metadata{Name: "wildcard", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{
			Models:     []string{"anthropic/*"},
			RateLimits: []policy.RateLimitLink{{RateLimitID: meta.NewID(), Models: []string{"anthropic/*"}}},
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate = %v, want a provider wildcard accepted", err)
	}
}

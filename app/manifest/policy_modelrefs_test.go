package manifest_test

import (
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
)

const rlUUID = "0195f8a0-0000-7000-8000-000000000033"

var polResolver = manifest.MapResolver{
	RateLimits: map[string]string{"burst": rlUUID},
}

// apply stores model refs in the canonical form the control API
// stores them in. Anything else is perpetual drift: apply would report an
// update on every run against a row the API just normalised.
func TestToPolicy_CanonicalisesModelRefs(t *testing.T) {
	d := manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "p", Owner: manifest.WireOwner{Kind: meta.OwnerSystem}},
		Spec: manifest.PolicySpec{
			Models: []string{"OpenAI/GPT-4o", "openai/gpt-4o", "Anthropic/claude-3.5"},
			RateLimits: []manifest.PolicyRateLimitDTO{
				{Ref: "burst", Models: []string{"OpenAI/GPT-4o"}},
			},
		},
	}
	p, err := manifest.ToPolicy(d, polResolver)
	if err != nil {
		t.Fatalf("ToPolicy: %v", err)
	}
	want := []string{"openai/gpt-4o", "anthropic/claude-3-5"}
	if len(p.Spec.Models) != len(want) {
		t.Fatalf("models = %v, want %v", p.Spec.Models, want)
	}
	for i := range want {
		if p.Spec.Models[i] != want[i] {
			t.Fatalf("models = %v, want %v", p.Spec.Models, want)
		}
	}
	if len(p.Spec.RateLimits) != 1 || p.Spec.RateLimits[0].Models[0] != "openai/gpt-4o" {
		t.Fatalf("rateLimits models = %v, want [openai/gpt-4o]", p.Spec.RateLimits)
	}
}

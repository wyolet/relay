package manifest_test

import (
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

const rlUUID = "0195f8a0-0000-7000-8000-000000000033"

var polResolver = manifest.MapResolver{
	RateLimits: map[string]string{"burst": rlUUID},
}

var polRev = manifest.MapReverseResolver{
	RateLimits: map[string]string{rlUUID: "burst"},
	Models:     map[string]string{"m-1": "gpt-4o"},
	Providers:  map[string]string{"prov-1": "openai"},
}

// apply stores model refs in the canonical form the control API
// stores them in. Anything else is perpetual drift: apply would report an
// update on every run against a row the API just normalised.
func TestToPolicy_CanonicalisesModelRefs(t *testing.T) {
	d := manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "p", Owner: manifest.WireOwner{Kind: meta.OwnerSystem}},
		Spec: manifest.PolicySpec{
			Models: []string{"OpenAI/GPT-4o", "openai/gpt-4o", "Anthropic/claude-3.5"},
			RLBindings: []manifest.RLBindingDTO{
				{Models: []string{"OpenAI/GPT-4o"}, RateLimit: "burst"},
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
	if len(p.Spec.RLBindings) != 1 || p.Spec.RLBindings[0].Models[0] != "openai/gpt-4o" {
		t.Fatalf("rlBinding models = %v, want [openai/gpt-4o]", p.Spec.RLBindings[0].Models)
	}
}

// A row carrying both grant fields renders only Models: emitting the
// legacy ModelIDs alongside them would widen the grant on the next apply.
func TestFromPolicy_RendersModelIDsOnlyWhenModelsIsEmpty(t *testing.T) {
	p := &policy.Policy{
		Meta: meta.Metadata{ID: "pol-1", Name: "p", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{Models: []string{"openai/gpt-4o"}, ModelIDs: []string{"m-1"}},
	}
	if got := manifest.FromPolicy(p, polRev); len(got.Spec.Models) != 1 || got.Spec.Models[0] != "openai/gpt-4o" {
		t.Fatalf("models = %v, want only the DSL grant", got.Spec.Models)
	}
	p.Spec.Models = nil
	if got := manifest.FromPolicy(p, polRev); len(got.Spec.Models) != 1 {
		t.Fatalf("models = %v, want the legacy grant rendered when Models is empty", got.Spec.Models)
	}
}

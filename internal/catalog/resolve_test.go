package catalog

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/ids"
)

func TestResolveRefs_RewritesProviderNameToID(t *testing.T) {
	provID := ids.New()
	snap := newSnapshot()
	snap.providers["openai"] = &Provider{Metadata: Metadata{ID: provID, Name: "openai"}}
	snap.secrets["k"] = &Secret{Metadata: Metadata{ID: ids.New(), Name: "k"}, Spec: SecretSpec{Provider: "openai"}}
	snap.policies["p"] = &Policy{Metadata: Metadata{ID: ids.New(), Name: "p"}, Spec: PolicySpec{Provider: "openai"}}
	snap.models["m"] = &Model{Metadata: Metadata{ID: ids.New(), Name: "m"}, Spec: ModelSpec{Provider: "openai", UpstreamName: "m"}}
	snap.buildByIDIndexes()

	mutated, err := resolveRefs(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !mutated.secrets || !mutated.policies || !mutated.models {
		t.Errorf("expected all three kinds mutated, got %+v", mutated)
	}
	if snap.secrets["k"].Spec.Provider != provID {
		t.Errorf("secret provider: got %q, want id %q", snap.secrets["k"].Spec.Provider, provID)
	}
	if snap.policies["p"].Spec.Provider != provID {
		t.Errorf("policy provider: got %q, want id %q", snap.policies["p"].Spec.Provider, provID)
	}
	if snap.models["m"].Spec.Provider != provID {
		t.Errorf("model provider: got %q, want id %q", snap.models["m"].Spec.Provider, provID)
	}
}

func TestResolveRefs_IdempotentWhenAlreadyID(t *testing.T) {
	provID := ids.New()
	snap := newSnapshot()
	snap.providers["openai"] = &Provider{Metadata: Metadata{ID: provID, Name: "openai"}}
	snap.secrets["k"] = &Secret{Metadata: Metadata{ID: ids.New(), Name: "k"}, Spec: SecretSpec{Provider: provID}}
	snap.buildByIDIndexes()

	mutated, err := resolveRefs(snap)
	if err != nil {
		t.Fatal(err)
	}
	if mutated.any() {
		t.Errorf("expected no mutation, got %+v", mutated)
	}
}

func TestResolveRefs_RewritesPolicyNameToID(t *testing.T) {
	provID := ids.New()
	polID := ids.New()
	snap := newSnapshot()
	snap.providers["openai"] = &Provider{Metadata: Metadata{ID: provID, Name: "openai"}, Spec: ProviderSpec{DefaultPolicy: "main"}}
	snap.policies["main"] = &Policy{Metadata: Metadata{ID: polID, Name: "main"}, Spec: PolicySpec{Provider: provID}}
	snap.relayKeys["k"] = &RelayKey{Metadata: Metadata{ID: ids.New(), Name: "k"}, Spec: RelayKeySpec{PolicyRef: "main"}}
	snap.buildByIDIndexes()

	mutated, err := resolveRefs(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !mutated.providers || !mutated.relayKeys {
		t.Errorf("expected providers+relayKeys mutated, got %+v", mutated)
	}
	if snap.providers["openai"].Spec.DefaultPolicy != polID {
		t.Errorf("defaultPolicy: got %q, want id %q", snap.providers["openai"].Spec.DefaultPolicy, polID)
	}
	if snap.relayKeys["k"].Spec.PolicyRef != polID {
		t.Errorf("policyRef: got %q, want id %q", snap.relayKeys["k"].Spec.PolicyRef, polID)
	}
}

func TestResolveRefs_RewritesModelNamesToIDs(t *testing.T) {
	provID := ids.New()
	mID := ids.New()
	replID := ids.New()
	snap := newSnapshot()
	snap.providers["openai"] = &Provider{Metadata: Metadata{ID: provID, Name: "openai"}}
	snap.models["gpt-4"] = &Model{Metadata: Metadata{ID: mID, Name: "gpt-4"}, Spec: ModelSpec{Provider: provID, UpstreamName: "gpt-4", Deprecation: &Deprecation{Replacement: "gpt-5"}}}
	snap.models["gpt-5"] = &Model{Metadata: Metadata{ID: replID, Name: "gpt-5"}, Spec: ModelSpec{Provider: provID, UpstreamName: "gpt-5"}}
	snap.policies["p"] = &Policy{Metadata: Metadata{ID: ids.New(), Name: "p"}, Spec: PolicySpec{Provider: provID, Models: []string{"gpt-4"}}}
	snap.routes["r"] = &Route{Metadata: Metadata{ID: ids.New(), Name: "r"}, Spec: RouteSpec{Models: []string{"gpt-4", "gpt-5"}}}
	snap.buildByIDIndexes()

	if _, err := resolveRefs(snap); err != nil {
		t.Fatal(err)
	}
	if snap.policies["p"].Spec.Models[0] != mID {
		t.Errorf("policy.models[0]: got %q, want %q", snap.policies["p"].Spec.Models[0], mID)
	}
	if snap.routes["r"].Spec.Models[0] != mID || snap.routes["r"].Spec.Models[1] != replID {
		t.Errorf("route.models: %+v", snap.routes["r"].Spec.Models)
	}
	if snap.models["gpt-4"].Spec.Deprecation.Replacement != replID {
		t.Errorf("deprecation.replacement: got %q, want %q", snap.models["gpt-4"].Spec.Deprecation.Replacement, replID)
	}
}

func TestResolveRefs_UnknownProviderErrors(t *testing.T) {
	snap := newSnapshot()
	snap.providers["openai"] = &Provider{Metadata: Metadata{ID: ids.New(), Name: "openai"}}
	snap.secrets["k"] = &Secret{Metadata: Metadata{ID: ids.New(), Name: "k"}, Spec: SecretSpec{Provider: "ghost"}}
	snap.buildByIDIndexes()

	_, err := resolveRefs(snap)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

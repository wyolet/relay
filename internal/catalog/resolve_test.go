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

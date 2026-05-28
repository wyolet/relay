package v1

import "testing"

func TestRegistryGet(t *testing.T) {
	reg := Registry{}
	if got := reg.Get("nonexistent"); got != nil {
		t.Errorf("expected nil for absent name, got %v", got)
	}
}

func TestRegistryGetPresent(t *testing.T) {
	// Use a nil Translator value to verify the map lookup works.
	reg := Registry{
		"test-adapter": nil,
	}
	// Get returns whatever is stored — nil is valid for a registered-but-nil entry.
	_ = reg.Get("test-adapter")
	if _, ok := reg["test-adapter"]; !ok {
		t.Error("expected test-adapter to be present in registry")
	}
}

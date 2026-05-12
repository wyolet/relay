package catalog

import (
	"testing"
	"time"
)

// TestUpstreamTierAutoInjection verifies the snapshot loader injects
// system_mirrored RateLimits for secrets with a resolvable upstream tier.
func TestUpstreamTierAutoInjection(t *testing.T) {
	// Provider with DefaultTier="openai-tier-3"; two secrets:
	//   - s1 overrides to "openai-tier-5"
	//   - s2 inherits provider default ("openai-tier-3")
	prov := &Provider{
		APIVersion: APIVersion,
		Kind:       KindProvider,
		Metadata:   Metadata{Name: "openai"},
		Spec: ProviderSpec{
			Kind:        PKOpenAI,
			BaseURL:     "https://api.openai.com",
			DefaultTier: "openai-tier-3",
		},
	}
	s1 := &Secret{
		APIVersion: APIVersion,
		Kind:       KindSecret,
		Metadata:   Metadata{Name: "key-a"},
		Spec: SecretSpec{
			Provider: "openai",
			Tier:     "openai-tier-5",
		},
		Resolved: "sk-test-a",
	}
	s2 := &Secret{
		APIVersion: APIVersion,
		Kind:       KindSecret,
		Metadata:   Metadata{Name: "key-b"},
		Spec: SecretSpec{
			Provider: "openai",
			// no Tier; should inherit DefaultTier="openai-tier-3"
		},
		Resolved: "sk-test-b",
	}

	snap := newSnapshot()
	snap.providers["openai"] = prov
	snap.secrets["key-a"] = s1
	snap.secrets["key-b"] = s2
	snap.injectUpstreamTierRateLimits()

	// Both secrets should have an auto-injected RL in secretTierRLs.
	if _, ok := snap.secretTierRLs["key-a"]; !ok {
		t.Fatal("expected auto-injected RL for key-a")
	}
	if _, ok := snap.secretTierRLs["key-b"]; !ok {
		t.Fatal("expected auto-injected RL for key-b")
	}

	// Verify correct tier names.
	rlA := snap.secretTierRLs["key-a"]
	if rlA.Metadata.Name != "upstream-key-a-openai-tier-5" {
		t.Errorf("key-a RL name: got %q, want %q", rlA.Metadata.Name, "upstream-key-a-openai-tier-5")
	}
	rlB := snap.secretTierRLs["key-b"]
	if rlB.Metadata.Name != "upstream-key-b-openai-tier-3" {
		t.Errorf("key-b RL name: got %q, want %q", rlB.Metadata.Name, "upstream-key-b-openai-tier-3")
	}

	// Source must be system_mirrored.
	if rlA.Spec.Source != string(SourceSystemMirrored) {
		t.Errorf("key-a RL source: got %q, want system_mirrored", rlA.Spec.Source)
	}

	// Verify rule counts and values for tier-5 (10000 RPM, 30M TPM).
	if len(rlA.Spec.Rules) != 2 {
		t.Fatalf("key-a RL rules: got %d, want 2", len(rlA.Spec.Rules))
	}
	for _, r := range rlA.Spec.Rules {
		if r.Window != time.Minute {
			t.Errorf("key-a rule %s window: got %v, want 1m", r.Meter, r.Window)
		}
		if r.Strategy != StrategySlidingWindow {
			t.Errorf("key-a rule %s strategy: got %q, want sliding-window", r.Meter, r.Strategy)
		}
	}
	rulesA := rulesMap(rlA.Spec.Rules)
	if rulesA["requests"] != 10000 {
		t.Errorf("key-a requests limit: got %d, want 10000", rulesA["requests"])
	}
	if rulesA["tokens"] != 30000000 {
		t.Errorf("key-a tokens limit: got %d, want 30000000", rulesA["tokens"])
	}

	// Verify rule counts and values for tier-3 (5000 RPM, 800k TPM).
	rulesB := rulesMap(rlB.Spec.Rules)
	if rulesB["requests"] != 5000 {
		t.Errorf("key-b requests limit: got %d, want 5000", rulesB["requests"])
	}
	if rulesB["tokens"] != 800000 {
		t.Errorf("key-b tokens limit: got %d, want 800000", rulesB["tokens"])
	}

	// Both RLs must also appear in the rateLimits map.
	if _, ok := snap.rateLimits[rlA.Metadata.Name]; !ok {
		t.Errorf("key-a auto-RL not in rateLimits map")
	}
	if _, ok := snap.rateLimits[rlB.Metadata.Name]; !ok {
		t.Errorf("key-b auto-RL not in rateLimits map")
	}
}

// TestUpstreamTierRateLimitsForRequest verifies that rateLimitsForRequest
// returns the auto-injected rules for a secret.
func TestUpstreamTierRateLimitsForRequest(t *testing.T) {
	prov := &Provider{
		APIVersion: APIVersion,
		Kind:       KindProvider,
		Metadata:   Metadata{Name: "openai"},
		Spec: ProviderSpec{
			Kind:        PKOpenAI,
			BaseURL:     "https://api.openai.com",
			DefaultTier: "openai-tier-2",
		},
	}
	sec := &Secret{
		APIVersion: APIVersion,
		Kind:       KindSecret,
		Metadata:   Metadata{Name: "my-key"},
		Spec: SecretSpec{
			Provider: "openai",
		},
		Resolved: "sk-x",
	}

	snap := newSnapshot()
	snap.providers["openai"] = prov
	snap.secrets["my-key"] = sec
	snap.injectUpstreamTierRateLimits()

	rules := snap.rateLimitsForRequest(prov, nil, nil, sec)
	if len(rules) == 0 {
		t.Fatal("expected auto-injected rules but got none")
	}
	// Should have 2 rules (requests + tokens) from tier-2.
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	for _, rr := range rules {
		if rr.RateLimitName != "upstream-my-key-openai-tier-2" {
			t.Errorf("rule RateLimitName: got %q", rr.RateLimitName)
		}
	}
}

// TestUpstreamTierUnknown verifies that an unknown tier name is silently skipped
// (no auto-injection, no panic).
func TestUpstreamTierUnknown(t *testing.T) {
	prov := &Provider{
		APIVersion: APIVersion,
		Kind:       KindProvider,
		Metadata:   Metadata{Name: "openai"},
		Spec: ProviderSpec{
			Kind:    PKOpenAI,
			BaseURL: "https://api.openai.com",
		},
	}
	sec := &Secret{
		APIVersion: APIVersion,
		Kind:       KindSecret,
		Metadata:   Metadata{Name: "mystery-key"},
		Spec: SecretSpec{
			Provider: "openai",
			Tier:     "openai-tier-99", // non-existent
		},
		Resolved: "sk-z",
	}
	snap := newSnapshot()
	snap.providers["openai"] = prov
	snap.secrets["mystery-key"] = sec
	snap.injectUpstreamTierRateLimits() // must not panic

	if _, ok := snap.secretTierRLs["mystery-key"]; ok {
		t.Error("expected no auto-injection for unknown tier, but got one")
	}
}

// TestUpstreamTierNoTier verifies that a secret with no tier and a provider
// with no default produces no auto-injection.
func TestUpstreamTierNoTier(t *testing.T) {
	prov := &Provider{
		APIVersion: APIVersion,
		Kind:       KindProvider,
		Metadata:   Metadata{Name: "openai"},
		Spec: ProviderSpec{
			Kind:    PKOpenAI,
			BaseURL: "https://api.openai.com",
			// no DefaultTier
		},
	}
	sec := &Secret{
		APIVersion: APIVersion,
		Kind:       KindSecret,
		Metadata:   Metadata{Name: "bare-key"},
		Spec: SecretSpec{
			Provider: "openai",
			// no Tier
		},
		Resolved: "sk-y",
	}
	snap := newSnapshot()
	snap.providers["openai"] = prov
	snap.secrets["bare-key"] = sec
	snap.injectUpstreamTierRateLimits()

	if _, ok := snap.secretTierRLs["bare-key"]; ok {
		t.Error("expected no auto-injection when tier is absent, but got one")
	}
	if len(snap.rateLimits) != 0 {
		t.Errorf("expected no RateLimits, got %d", len(snap.rateLimits))
	}
}

// rulesMap converts a rule slice to meter→amount map for easy assertions.
func rulesMap(rules []RateLimitRule) map[string]int64 {
	m := make(map[string]int64, len(rules))
	for _, r := range rules {
		m[r.Meter] = r.Amount
	}
	return m
}

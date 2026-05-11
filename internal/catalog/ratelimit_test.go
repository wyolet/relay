package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const rlOpenAIProvider = `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: openai-prod
spec:
  kind: openai
  baseURL: https://api.openai.com
`

const rlOllamaProvider = `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: dev-ollama
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
`

func TestRateLimit_LoadValidation(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: test-rl
spec:
  strategy: sliding-window
  window: 1m
  amount: 100
`)
		s, err := LoadYAML(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rl, ok := s.RateLimitByName("test-rl")
		if !ok {
			t.Fatal("rate limit not found")
		}
		if rl.Spec.Window != time.Minute {
			t.Errorf("expected window 1m, got %v", rl.Spec.Window)
		}
		if len(rl.Spec.Rules) != 1 || rl.Spec.Rules[0].Amount != 100 {
			t.Errorf("expected lifted rules[0].Amount=100, got %+v", rl.Spec.Rules)
		}
		if rl.Spec.Source != "" {
			t.Errorf("expected empty spec.Source, got %q", rl.Spec.Source)
		}
	})

	t.Run("negative amount", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: bad-rl
spec:
  strategy: sliding-window
  window: 1m
  amount: -1
`)
		_, err := LoadYAML(dir)
		if err == nil || !strings.Contains(err.Error(), "amount must be > 0") {
			t.Fatalf("expected amount error, got: %v", err)
		}
	})

	t.Run("zero window", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: bad-rl
spec:
  strategy: sliding-window
  window: 0s
  amount: 100
`)
		_, err := LoadYAML(dir)
		if err == nil || !strings.Contains(err.Error(), "window must be > 0") {
			t.Fatalf("expected window error, got: %v", err)
		}
	})

	t.Run("unknown strategy", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: bad-rl
spec:
  window: 1m
  rules:
    - meter: requests
      amount: 100
      strategy: totally-unknown
`)
		_, err := LoadYAML(dir)
		if err == nil || !strings.Contains(err.Error(), "unsupported strategy") {
			t.Fatalf("expected strategy error, got: %v", err)
		}
	})

	t.Run("all known strategies valid", func(t *testing.T) {
		for _, strat := range []string{"sliding-window", "fixed-window", "token-bucket", "leaky-bucket"} {
			dir := t.TempDir()
			writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: strat-rl
spec:
  window: 1m
  rules:
    - meter: requests
      amount: 100
      strategy: `+strat+`
`)
			if _, err := LoadYAML(dir); err != nil {
				t.Errorf("strategy %q: unexpected error: %v", strat, err)
			}
		}
	})
}

func TestRateLimitAttachment_RefResolution(t *testing.T) {
	// Test both new string form and legacy {ref,meter} form — both should parse.
	t.Setenv("KEY", "val")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: req-rl
spec:
  strategy: sliding-window
  window: 30s
  meter: requests
  amount: 50
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: con-rl
spec:
  strategy: sliding-window
  window: 30s
  meter: concurrency
  amount: 10
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: anon-sec
spec:
  provider: dev-ollama
  value: ""
  rateLimits:
    - req-rl
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: ollama-policy
spec:
  provider: dev-ollama
  secrets:
    - anon-sec
  rateLimits:
    - ref: req-rl
      meter: tokens
---
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: llama3
spec:
  provider: dev-ollama
  upstreamName: llama3
  rateLimits:
    - con-rl
`)
	_, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRateLimitAttachment_UnknownRef(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: anon-sec
spec:
  provider: dev-ollama
  value: ""
  rateLimits:
    - ref: nonexistent-rl
      meter: requests
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected unknown ref error, got: %v", err)
	}
}

func TestRateLimitAttachment_BadMeter(t *testing.T) {
	// Bad meter now lives in the RateLimit spec itself, not in the attachment.
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: my-rl
spec:
  strategy: sliding-window
  window: 1m
  meter: xyz
  amount: 100
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "meter") {
		t.Fatalf("expected meter error, got: %v", err)
	}
}

func TestRateLimitAttachment_LegacyShape_Accepted(t *testing.T) {
	// Old {ref, meter} attachment shape still parses; meter on attachment is ignored.
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: my-rl
spec:
  strategy: sliding-window
  window: 30s
  meter: requests
  amount: 50
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: anon-sec
spec:
  provider: dev-ollama
  value: ""
  rateLimits:
    - ref: my-rl
      meter: requests
`)
	_, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("expected legacy attachment to parse without error, got: %v", err)
	}
}

func TestAuthRequiredPool_NoLimits_Fails(t *testing.T) {
	t.Setenv("KEY", "sk-test")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOpenAIProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: my-key
spec:
  provider: openai-prod
  valueFrom:
    env: KEY
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: my-policy
spec:
  provider: openai-prod
  secrets:
    - my-key
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "auth-required") {
		t.Fatalf("expected auth-required rate-limit error, got: %v", err)
	}
}

func TestAuthRequiredPool_SkipDefaultLimits_Allows(t *testing.T) {
	t.Setenv("KEY", "sk-test")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOpenAIProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: my-key
spec:
  provider: openai-prod
  valueFrom:
    env: KEY
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: my-policy
spec:
  provider: openai-prod
  secrets:
    - my-key
  skipDefaultLimits: true
`)
	_, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAnonymousPool_NoLimits_OK(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: anon
spec:
  provider: dev-ollama
  value: ""
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: ollama-policy
spec:
  provider: dev-ollama
  secrets:
    - anon
`)
	_, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRateLimitsForRequest_ReturnsUnion(t *testing.T) {
	t.Setenv("KEY", "sk-test")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOpenAIProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rpm-rl
spec:
  strategy: sliding-window
  window: 1m
  meter: requests
  amount: 1000
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: tpm-rl
spec:
  strategy: sliding-window
  window: 1m
  meter: tokens
  amount: 100000
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: my-key
spec:
  provider: openai-prod
  valueFrom:
    env: KEY
  rateLimits:
    - rpm-rl
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: my-policy
spec:
  provider: openai-prod
  secrets:
    - my-key
  skipDefaultLimits: true
---
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: gpt-4o
spec:
  provider: openai-prod
  upstreamName: gpt-4o
  rateLimits:
    - tpm-rl
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sec, _ := s.SecretByName("my-key")
	policy, _ := s.PolicyByName("my-policy")
	model, _ := s.ModelByName("gpt-4o")
	rules := s.RateLimitsForRequest(nil, policy, model, sec)
	if len(rules) != 2 {
		t.Fatalf("expected 2 resolved rules, got %d", len(rules))
	}
	if rules[0].ParentKind != KindSecret || rules[0].Meter != MeterRequests {
		t.Errorf("expected first rule to be Secret/requests, got %v/%v", rules[0].ParentKind, rules[0].Meter)
	}
	if rules[1].ParentKind != KindModel || rules[1].Meter != MeterTokens {
		t.Errorf("expected second rule to be Model/tokens, got %v/%v", rules[1].ParentKind, rules[1].Meter)
	}
}

func TestRateLimitsForRequest_DeterministicOrder(t *testing.T) {
	t.Setenv("KEY", "sk-test")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOpenAIProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rl-a
spec:
  strategy: sliding-window
  window: 1m
  meter: requests
  amount: 500
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rl-b
spec:
  strategy: sliding-window
  window: 1m
  meter: tokens
  amount: 1000
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rl-c
spec:
  strategy: sliding-window
  window: 1m
  meter: concurrency
  amount: 5000
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: my-key
spec:
  provider: openai-prod
  valueFrom:
    env: KEY
  rateLimits:
    - rl-a
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: my-policy
spec:
  provider: openai-prod
  secrets:
    - my-key
  rateLimits:
    - rl-b
  skipDefaultLimits: true
---
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: gpt-4o
spec:
  provider: openai-prod
  upstreamName: gpt-4o
  rateLimits:
    - rl-c
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sec, _ := s.SecretByName("my-key")
	policy, _ := s.PolicyByName("my-policy")
	model, _ := s.ModelByName("gpt-4o")
	rules := s.RateLimitsForRequest(nil, policy, model, sec)
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}
	if rules[0].ParentKind != KindSecret {
		t.Errorf("first rule should be Secret, got %v", rules[0].ParentKind)
	}
	if rules[1].ParentKind != KindPolicy {
		t.Errorf("second rule should be Policy, got %v", rules[1].ParentKind)
	}
	if rules[2].ParentKind != KindModel {
		t.Errorf("third rule should be Model, got %v", rules[2].ParentKind)
	}
}

// TestMultiRuleRateLimit_ParsesAndExpands verifies a multi-rule RateLimit produces
// one ResolvedRule per rule when expanded via RateLimitsForRequest.
func TestMultiRuleRateLimit_ParsesAndExpands(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: tier-1
spec:
  strategy: sliding-window
  window: 1m
  rules:
    - meter: requests
      amount: 5
    - meter: tokens.input
      amount: 200000
    - meter: tokens.output
      amount: 50000
---
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: ollama-policy
spec:
  provider: dev-ollama
  rateLimits:
    - tier-1
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	policy, _ := s.PolicyByName("ollama-policy")
	rules := s.RateLimitsForRequest(nil, policy, nil, nil)
	if len(rules) != 3 {
		t.Fatalf("expected 3 resolved rules (one per Rule in tier-1), got %d", len(rules))
	}
	meters := make([]string, len(rules))
	for i, r := range rules {
		meters[i] = r.Rule.Meter
	}
	want := []string{"requests", "tokens.input", "tokens.output"}
	for i, m := range want {
		if meters[i] != m {
			t.Errorf("rule[%d]: expected meter %q, got %q", i, m, meters[i])
		}
	}
	// Verify amounts
	if rules[0].Rule.Amount != 5 {
		t.Errorf("rule[0] amount: expected 5, got %d", rules[0].Rule.Amount)
	}
	if rules[1].Rule.Amount != 200000 {
		t.Errorf("rule[1] amount: expected 200000, got %d", rules[1].Rule.Amount)
	}
	if rules[2].Rule.Amount != 50000 {
		t.Errorf("rule[2] amount: expected 50000, got %d", rules[2].Rule.Amount)
	}
	// Verify Window is propagated to each ResolvedRule
	for i, r := range rules {
		if r.Window != time.Minute {
			t.Errorf("rule[%d]: expected window 1m, got %v", i, r.Window)
		}
	}
}

// TestRateLimitSpec_LegacyJSONLift verifies that legacy top-level shape on
// the wire is lifted into rules[] by UnmarshalJSON.
func TestRateLimitSpec_LegacyJSONLift(t *testing.T) {
	var spec RateLimitSpec
	if err := json.Unmarshal([]byte(`{"strategy":"sliding-window","window":60000000000,"amount":100,"meter":"tokens"}`), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(spec.Rules) != 1 || spec.Rules[0].Meter != "tokens" || spec.Rules[0].Amount != 100 {
		t.Errorf("expected single tokens rule with amount 100, got %+v", spec.Rules)
	}
}

// TestRateLimitSpec_WindowStringParsed verifies that JSON window accepts "1m" strings.
func TestRateLimitSpec_WindowStringParsed(t *testing.T) {
	var spec RateLimitSpec
	if err := json.Unmarshal([]byte(`{"strategy":"sliding-window","window":"1m","amount":10}`), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Window != time.Minute {
		t.Errorf("expected 1m, got %v", spec.Window)
	}
	// also 30s
	if err := json.Unmarshal([]byte(`{"strategy":"sliding-window","window":"30s","amount":10}`), &spec); err != nil {
		t.Fatalf("unmarshal 30s: %v", err)
	}
	if spec.Window != 30*time.Second {
		t.Errorf("expected 30s, got %v", spec.Window)
	}
	// nanoseconds-as-int still works (legacy storage format)
	if err := json.Unmarshal([]byte(`{"strategy":"sliding-window","window":60000000000,"amount":10}`), &spec); err != nil {
		t.Fatalf("unmarshal int: %v", err)
	}
	if spec.Window != time.Minute {
		t.Errorf("expected 1m from int, got %v", spec.Window)
	}
}

// TestRateLimitSpec_LegacyDefaultMeter verifies empty legacy Meter defaults to "requests".
func TestRateLimitSpec_LegacyDefaultMeter(t *testing.T) {
	var spec RateLimitSpec
	if err := json.Unmarshal([]byte(`{"strategy":"sliding-window","window":60000000000,"amount":50}`), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(spec.Rules) != 1 || spec.Rules[0].Meter != "requests" {
		t.Fatalf("expected single requests rule, got %+v", spec.Rules)
	}
}

// TestMultiRuleRateLimit_ValidationErrors verifies per-rule validation.
func TestMultiRuleRateLimit_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{
			name: "invalid meter",
			spec: `rules:
    - meter: xyz
      amount: 100`,
			wantErr: "meter",
		},
		{
			name: "zero amount in rule",
			spec: `rules:
    - meter: requests
      amount: 0`,
			wantErr: "amount must be > 0",
		},
		{
			name: "bad source",
			spec: `source: not-attribution
  rules:
    - meter: requests
      amount: 10`,
			wantErr: "source",
		},
		{
			name:    "empty rules — must provide legacy fields",
			spec:    `rules: []`,
			wantErr: "rules must be non-empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: bad-rl
spec:
  strategy: sliding-window
  window: 1m
  `+tc.spec+`
`)
			_, err := LoadYAML(dir)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

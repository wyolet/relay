package catalog

import (
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
		if rl.Spec.Amount != 100 {
			t.Errorf("expected amount 100, got %d", rl.Spec.Amount)
		}
		if rl.Spec.Source != "" {
			t.Errorf("expected empty source, got %q", rl.Spec.Source)
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
  strategy: fixed-window
  window: 1m
  amount: 100
`)
		_, err := LoadYAML(dir)
		if err == nil || !strings.Contains(err.Error(), "unsupported strategy") {
			t.Fatalf("expected strategy error, got: %v", err)
		}
	})
}

func TestRateLimitAttachment_RefResolution(t *testing.T) {
	t.Setenv("KEY", "val")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: my-rl
spec:
  strategy: sliding-window
  window: 30s
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
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: ollama-pool
spec:
  provider: dev-ollama
  secrets:
    - anon-sec
  rateLimits:
    - ref: my-rl
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
    - ref: my-rl
      meter: concurrency
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
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", rlOllamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: my-rl
spec:
  strategy: sliding-window
  window: 1m
  amount: 100
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
      meter: xyz
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "meter") {
		t.Fatalf("expected meter error, got: %v", err)
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
kind: Pool
metadata:
  name: my-pool
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
kind: Pool
metadata:
  name: my-pool
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
kind: Pool
metadata:
  name: ollama-pool
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
  amount: 1000
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: tpm-rl
spec:
  strategy: sliding-window
  window: 1m
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
    - ref: rpm-rl
      meter: requests
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: my-pool
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
    - ref: tpm-rl
      meter: tokens
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sec, _ := s.SecretByName("my-key")
	pool, _ := s.PoolByName("my-pool")
	model, _ := s.ModelByName("gpt-4o")
	rules := s.RateLimitsForRequest(nil, pool, model, sec)
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
  amount: 500
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rl-b
spec:
  strategy: sliding-window
  window: 1m
  amount: 1000
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: rl-c
spec:
  strategy: sliding-window
  window: 1m
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
    - ref: rl-a
      meter: requests
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: my-pool
spec:
  provider: openai-prod
  secrets:
    - my-key
  rateLimits:
    - ref: rl-b
      meter: tokens
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
    - ref: rl-c
      meter: concurrency
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sec, _ := s.SecretByName("my-key")
	pool, _ := s.PoolByName("my-pool")
	model, _ := s.ModelByName("gpt-4o")
	rules := s.RateLimitsForRequest(nil, pool, model, sec)
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}
	if rules[0].ParentKind != KindSecret {
		t.Errorf("first rule should be Secret, got %v", rules[0].ParentKind)
	}
	if rules[1].ParentKind != KindPool {
		t.Errorf("second rule should be Pool, got %v", rules[1].ParentKind)
	}
	if rules[2].ParentKind != KindModel {
		t.Errorf("third rule should be Model, got %v", rules[2].ParentKind)
	}
}

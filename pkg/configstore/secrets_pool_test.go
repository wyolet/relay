package configstore

import (
	"strings"
	"testing"
)

// minProvider returns a minimal ollama provider YAML doc.
const ollamaProvider = `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: dev-ollama
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
`

const openaiProvider = `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: openai-prod
spec:
  kind: openai
  baseURL: https://api.openai.com
`

func TestSecret_EnvResolution(t *testing.T) {
	t.Setenv("OPENAI_KEY", "sk-test-value")
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", openaiProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: my-key
spec:
  provider: openai-prod
  valueFrom:
    env: OPENAI_KEY
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sec, ok := s.SecretByName("my-key")
	if !ok {
		t.Fatal("secret not found")
	}
	if sec.Resolved != "sk-test-value" {
		t.Errorf("expected resolved sk-test-value, got %q", sec.Resolved)
	}
	if sec.KeyHash == "" {
		t.Error("KeyHash should be populated")
	}
	if sec.UsedLiteral {
		t.Error("UsedLiteral should be false")
	}
}

func TestSecret_LiteralValue(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", ollamaProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: anon-key
spec:
  provider: dev-ollama
  value: my-literal-secret
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sec, ok := s.SecretByName("anon-key")
	if !ok {
		t.Fatal("secret not found")
	}
	if sec.Resolved != "my-literal-secret" {
		t.Errorf("expected resolved my-literal-secret, got %q", sec.Resolved)
	}
	if !sec.UsedLiteral {
		t.Error("UsedLiteral should be true")
	}
}

func TestSecret_BothFieldsSet(t *testing.T) {
	t.Setenv("OPENAI_KEY", "sk-test")
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", openaiProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: bad-key
spec:
  provider: openai-prod
  valueFrom:
    env: OPENAI_KEY
  value: literal
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected both-fields error, got: %v", err)
	}
}

func TestSecret_NeitherFieldSet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", openaiProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: empty-key
spec:
  provider: openai-prod
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required error, got: %v", err)
	}
}

func TestSecret_MissingEnvVar(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", openaiProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: missing-env-key
spec:
  provider: openai-prod
  valueFrom:
    env: DOES_NOT_EXIST_XYZ
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "DOES_NOT_EXIST_XYZ") {
		t.Fatalf("expected env var error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing-env-key") {
		t.Fatalf("error should mention secret name, got: %v", err)
	}
}

func TestSecret_EmptyEnvVar(t *testing.T) {
	t.Setenv("EMPTY_KEY", "")
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", openaiProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: empty-env-key
spec:
  provider: openai-prod
  valueFrom:
    env: EMPTY_KEY
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "EMPTY_KEY") {
		t.Fatalf("expected empty env var error, got: %v", err)
	}
}

func TestSecret_UnknownProvider(t *testing.T) {
	t.Setenv("OPENAI_KEY", "sk-test")
	dir := t.TempDir()
	writeFile(t, dir, "p.yaml", openaiProvider)
	writeFile(t, dir, "s.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: orphan-key
spec:
  provider: nonexistent
  valueFrom:
    env: OPENAI_KEY
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got: %v", err)
	}
}

func TestPool_ExplicitList(t *testing.T) {
	t.Setenv("KEY1", "secret1")
	t.Setenv("KEY2", "secret2")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", openaiProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: key-b
spec:
  provider: openai-prod
  valueFrom:
    env: KEY2
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: key-a
spec:
  provider: openai-prod
  valueFrom:
    env: KEY1
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: my-pool
spec:
  provider: openai-prod
  secrets:
    - key-a
    - key-b
  skipDefaultLimits: true
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool, ok := s.PoolByName("my-pool")
	if !ok {
		t.Fatal("pool not found")
	}
	secs := s.SecretsForPool(pool)
	if len(secs) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(secs))
	}
	if secs[0].Metadata.Name != "key-a" || secs[1].Metadata.Name != "key-b" {
		t.Errorf("expected alphabetical order, got %s, %s", secs[0].Metadata.Name, secs[1].Metadata.Name)
	}
}

func TestPool_Selector(t *testing.T) {
	t.Setenv("KEY1", "secret1")
	t.Setenv("KEY2", "secret2")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", openaiProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: prod-key
  labels:
    tier: prod
spec:
  provider: openai-prod
  valueFrom:
    env: KEY1
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: dev-key
  labels:
    tier: dev
spec:
  provider: openai-prod
  valueFrom:
    env: KEY2
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: prod-pool
spec:
  provider: openai-prod
  secretSelector:
    tier: prod
  skipDefaultLimits: true
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool, _ := s.PoolByName("prod-pool")
	secs := s.SecretsForPool(pool)
	if len(secs) != 1 || secs[0].Metadata.Name != "prod-key" {
		t.Errorf("expected only prod-key, got %v", secs)
	}
}

func TestPool_BothExplicitAndSelector(t *testing.T) {
	t.Setenv("K1", "s1")
	t.Setenv("K2", "s2")
	t.Setenv("K3", "s3")
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", openaiProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: sec-a
  labels:
    env: prod
spec:
  provider: openai-prod
  valueFrom:
    env: K1
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: sec-b
  labels:
    env: prod
spec:
  provider: openai-prod
  valueFrom:
    env: K2
---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: sec-c
spec:
  provider: openai-prod
  valueFrom:
    env: K3
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: combo-pool
spec:
  provider: openai-prod
  secrets:
    - sec-c
  secretSelector:
    env: prod
  skipDefaultLimits: true
`)
	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool, _ := s.PoolByName("combo-pool")
	secs := s.SecretsForPool(pool)
	if len(secs) != 3 {
		t.Fatalf("expected 3 secrets (union), got %d", len(secs))
	}
}

func TestPool_ProviderMismatch(t *testing.T) {
	t.Setenv("KEY1", "s1")
	dir := t.TempDir()
	writeFile(t, dir, "p1.yaml", openaiProvider)
	writeFile(t, dir, "p2.yaml", ollamaProvider)
	writeFile(t, dir, "all.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: ollama-key
spec:
  provider: dev-ollama
  valueFrom:
    env: KEY1
---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: bad-pool
spec:
  provider: openai-prod
  secrets:
    - ollama-key
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "belongs to provider") {
		t.Fatalf("expected provider mismatch error, got: %v", err)
	}
}

func TestPool_AuthRequiredEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", openaiProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Pool
metadata:
  name: empty-pool
spec:
  provider: openai-prod
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "requires auth") {
		t.Fatalf("expected auth required error, got: %v", err)
	}
}

func TestPool_AnonymousEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", ollamaProvider+`---
apiVersion: relay.wyolet.dev/v1
kind: Secret
metadata:
  name: ollama-anon
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
    - ollama-anon
`)
	_, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error for anonymous ollama pool: %v", err)
	}
}

func TestProvider_DefaultPoolUnknown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: dev-ollama
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
  defaultPool: nonexistent-pool
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected defaultPool not found error, got: %v", err)
	}
}

func TestProvider_DefaultPoolWrongProvider(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "p1.yaml", openaiProvider)
	writeFile(t, dir, "p2.yaml", ollamaProvider)
	writeFile(t, dir, "all.yaml", `apiVersion: relay.wyolet.dev/v1
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
---
apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: extra-ollama
spec:
  kind: ollama
  baseURL: http://localhost:11435
  defaultPool: ollama-pool
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "belongs to provider") {
		t.Fatalf("expected wrong provider error, got: %v", err)
	}
}

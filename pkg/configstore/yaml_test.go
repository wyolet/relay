package configstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

const happyYAML = `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: ollama-local
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
---
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: llama3
spec:
  provider: ollama-local
  upstreamName: llama3
---
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: mistral
spec:
  provider: ollama-local
  upstreamName: mistral
---
apiVersion: relay.wyolet.dev/v1
kind: Route
metadata:
  name: default-route
spec:
  default: true
  models:
    - llama3
    - mistral
---
apiVersion: relay.wyolet.dev/v1
kind: RateLimit
metadata:
  name: ollama-rpm
spec:
  target:
    kind: Provider
    name: ollama-local
  rpm: 60
`

func TestHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", happyYAML)

	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p := s.DefaultProvider(); p == nil || p.Metadata.Name != "ollama-local" {
		t.Fatalf("wrong default provider: %v", p)
	}
	if r := s.DefaultRoute(); r == nil || r.Metadata.Name != "default-route" {
		t.Fatalf("wrong default route: %v", r)
	}
	if _, ok := s.ModelByName("llama3"); !ok {
		t.Fatal("llama3 not found")
	}
	if _, ok := s.RateLimitByName("ollama-rpm"); !ok {
		t.Fatal("ollama-rpm not found")
	}
}

func TestMissingName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: ""
spec:
  kind: ollama
  baseURL: http://localhost:11434
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "metadata.name required") {
		t.Fatalf("expected metadata.name error, got: %v", err)
	}
}

func TestUnsupportedAPIVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `apiVersion: relay.wyolet.dev/v99
kind: Provider
metadata:
  name: foo
spec:
  kind: ollama
  baseURL: http://localhost:11434
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "unsupported apiVersion") {
		t.Fatalf("expected apiVersion error, got: %v", err)
	}
}

func TestUnknownProviderRef(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: real-provider
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
`)
	writeFile(t, dir, "bad.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: ghost-model
spec:
  provider: nonexistent
  upstreamName: ghost
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got: %v", err)
	}
}

func TestUnknownModelInRoute(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: p
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
---
apiVersion: relay.wyolet.dev/v1
kind: Route
metadata:
  name: bad-route
spec:
  models:
    - does-not-exist
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("expected unknown model error, got: %v", err)
	}
}

func TestTwoDefaultProviders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", `apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: p1
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
---
apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: p2
spec:
  kind: ollama
  baseURL: http://localhost:11435
  default: true
`)
	_, err := LoadYAML(dir)
	if err == nil || !strings.Contains(err.Error(), "at most one Provider may be default") {
		t.Fatalf("expected default provider error, got: %v", err)
	}
}

func TestEnumerationOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", happyYAML)

	s, err := LoadYAML(dir)
	if err != nil {
		t.Fatal(err)
	}

	models := s.Models()
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].Metadata.Name >= models[1].Metadata.Name {
		t.Errorf("models not in alphabetical order: %s, %s", models[0].Metadata.Name, models[1].Metadata.Name)
	}
}

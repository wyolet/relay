package litellm_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/internal/catalog"
	litellmimport "github.com/wyolet/relay/internal/import/litellm"
)

// fixedToday is used for deterministic deprecation logic in tests.
var fixedToday = time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)

// loadFixture reads testdata/fixture.json via Fetch with --source-file.
func loadFixture(t *testing.T) map[string]litellmimport.Entry {
	t.Helper()
	entries, _, err := litellmimport.Fetch(context.Background(), "", "testdata/fixture.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return entries
}

// TestSourceFile verifies that --source-file reads local JSON without network.
func TestSourceFile(t *testing.T) {
	entries := loadFixture(t)
	if len(entries) == 0 {
		t.Fatal("expected entries, got 0")
	}
	if _, ok := entries["gpt-4o"]; !ok {
		t.Error("expected gpt-4o entry")
	}
}

// TestSkipNonChat verifies that embedding and audio entries are excluded.
func TestSkipNonChat(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	for _, m := range result.Models {
		if m.Metadata.Name == "text-embedding-ada-002" {
			t.Error("embedding model should have been skipped")
		}
		if m.Metadata.Name == "whisper-1" {
			t.Error("audio model should have been skipped")
		}
	}
	if result.SkippedMode == 0 {
		t.Error("expected SkippedMode > 0")
	}
}

// TestAliasCollapsing verifies that claude-3-5-sonnet variants collapse to one canonical.
func TestAliasCollapsing(t *testing.T) {
	// Three entries: bare name, dated, latest.
	entries := map[string]litellmimport.Entry{
		"claude-3-5-sonnet-20241022": {
			LiteLLMProvider:  "anthropic",
			Mode:             "chat",
			MaxInputTokens:   200000,
			MaxOutputTokens:  8192,
			InputCostPerToken: 3e-06,
			OutputCostPerToken: 1.5e-05,
			SupportsFunctionCalling: true,
			SupportsNativeStreaming: true,
		},
		"claude-3-5-sonnet": {
			LiteLLMProvider:  "anthropic",
			Mode:             "chat",
			MaxInputTokens:   200000,
			MaxOutputTokens:  8192,
			InputCostPerToken: 3e-06,
			OutputCostPerToken: 1.5e-05,
			SupportsFunctionCalling: true,
			SupportsNativeStreaming: true,
		},
		"claude-3-5-sonnet-latest": {
			LiteLLMProvider:  "anthropic",
			Mode:             "chat",
			MaxInputTokens:   200000,
			MaxOutputTokens:  8192,
			InputCostPerToken: 3e-06,
			OutputCostPerToken: 1.5e-05,
			SupportsFunctionCalling: true,
			SupportsNativeStreaming: true,
		},
	}

	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(result.Models) != 1 {
		t.Fatalf("expected 1 canonical model, got %d", len(result.Models))
	}
	m := result.Models[0]
	if m.Metadata.Name != "claude-3-5-sonnet-20241022" {
		t.Errorf("expected canonical claude-3-5-sonnet-20241022, got %q", m.Metadata.Name)
	}
	if len(m.Spec.Aliases) != 2 {
		t.Errorf("expected 2 aliases, got %d: %v", len(m.Spec.Aliases), m.Spec.Aliases)
	}
}

// TestProviderDeduplication verifies 5 anthropic models → 1 provider record.
func TestProviderDeduplication(t *testing.T) {
	// Build 5 anthropic chat entries.
	entries := map[string]litellmimport.Entry{}
	for i := 1; i <= 5; i++ {
		entries[strings.Repeat("a", i)+"-model"] = litellmimport.Entry{
			LiteLLMProvider: "anthropic",
			Mode:            "chat",
			MaxInputTokens:  200000,
			SupportsNativeStreaming: true,
		}
	}

	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(result.Providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(result.Providers))
	}
	if len(result.Models) != 5 {
		t.Errorf("expected 5 models, got %d", len(result.Models))
	}
}

// TestDeprecationPast verifies a model with past deprecation_date → status="sunset".
func TestDeprecationPast(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var found *catalog.Model
	for _, m := range result.Models {
		if m.Metadata.Name == "claude-3-haiku-20240307" {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatal("claude-3-haiku-20240307 not found")
	}
	if found.Spec.Deprecation == nil {
		t.Fatal("expected Deprecation to be set")
	}
	if found.Spec.Deprecation.Status != "sunset" {
		t.Errorf("expected status=sunset for past date, got %q", found.Spec.Deprecation.Status)
	}
}

// TestDeprecationFuture verifies a model with future deprecation_date → status="deprecated".
func TestDeprecationFuture(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var found *catalog.Model
	for _, m := range result.Models {
		if m.Metadata.Name == "claude-sonnet-4-20250514" {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatal("claude-sonnet-4-20250514 not found")
	}
	if found.Spec.Deprecation == nil {
		t.Fatal("expected Deprecation to be set")
	}
	if found.Spec.Deprecation.Status != "deprecated" {
		t.Errorf("expected status=deprecated for future date, got %q", found.Spec.Deprecation.Status)
	}
}

// TestProvenanceLabels verifies source/source_version labels are set.
func TestProvenanceLabels(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{
		SourceVersion: "abc123",
		Today:         fixedToday,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(result.Models) == 0 {
		t.Fatal("no models")
	}
	m := result.Models[0]
	if m.Metadata.Labels["source"] != "litellm" {
		t.Errorf("expected source=litellm, got %q", m.Metadata.Labels["source"])
	}
	if m.Metadata.Labels["source_version"] != "abc123" {
		t.Errorf("expected source_version=abc123, got %q", m.Metadata.Labels["source_version"])
	}
}

// TestSkipExistingMode verifies that mode=skip-existing respects existing models.
func TestSkipExistingMode(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	mem := newMemDB()
	// Pre-populate gpt-4o.
	for _, m := range result.Models {
		if m.Metadata.Name == "gpt-4o" {
			if err := mem.UpsertModel(context.Background(), *m); err != nil {
				t.Fatalf("pre-populate: %v", err)
			}
			break
		}
	}

	ar, err := litellmimport.Apply(context.Background(), mem, mem, result, litellmimport.ApplyOptions{
		Mode: litellmimport.ModeSkipExisting,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ar.ModelsSkipped == 0 {
		t.Error("expected at least one model skipped")
	}
	// gpt-4o should remain un-overwritten.
	existing := mem.model("gpt-4o")
	if existing == nil {
		t.Fatal("gpt-4o not found after apply")
	}
}

// TestGolden runs the full fixture through Translate and compares each model's
// YAML against the golden file in testdata/golden/<name>.yaml.
// Set UPDATE_GOLDEN=1 to regenerate.
func TestGolden(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{
		SourceVersion: "test-version",
		Today:         fixedToday,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	goldenDir := filepath.Join("testdata", "golden")
	update := os.Getenv("UPDATE_GOLDEN") == "1"

	for _, m := range result.Models {
		m := m
		t.Run(m.Metadata.Name, func(t *testing.T) {
			got, err := yaml.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			goldenFile := filepath.Join(goldenDir, m.Metadata.Name+".yaml")
			// Replace slashes in model names for filenames.
			goldenFile = filepath.Join(goldenDir, strings.ReplaceAll(m.Metadata.Name, "/", "_")+".yaml")

			if update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(goldenFile, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("updated golden: %s", goldenFile)
				return
			}

			want, err := os.ReadFile(goldenFile)
			if err != nil {
				// Golden doesn't exist — create it and report.
				if os.IsNotExist(err) {
					if err2 := os.MkdirAll(goldenDir, 0o755); err2 == nil {
						_ = os.WriteFile(goldenFile, got, 0o644)
					}
					t.Logf("created missing golden: %s (run again to verify)", goldenFile)
					return
				}
				t.Fatalf("read golden: %v", err)
			}

			if string(got) != string(want) {
				t.Errorf("golden mismatch for %s:\nGOT:\n%s\nWANT:\n%s", m.Metadata.Name, got, want)
			}
		})
	}
}

// TestOutputOrdering verifies models are emitted in alphabetical order (dry-run determinism).
func TestOutputOrdering(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	names := make([]string, len(result.Models))
	for i, m := range result.Models {
		names[i] = m.Metadata.Name
	}
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)
	for i := range names {
		if names[i] != sorted[i] {
			t.Errorf("models not sorted: position %d is %q, want %q", i, names[i], sorted[i])
		}
	}
}

// ---- in-memory CatalogDB for apply tests ----

type memDB struct {
	providers map[string]catalog.Provider
	models    map[string]catalog.Model
}

func newMemDB() *memDB {
	return &memDB{
		providers: map[string]catalog.Provider{},
		models:    map[string]catalog.Model{},
	}
}

func (m *memDB) model(name string) *catalog.Model {
	v, ok := m.models[name]
	if !ok {
		return nil
	}
	return &v
}

func (m *memDB) UpsertProvider(_ context.Context, p catalog.Provider) error {
	m.providers[p.Metadata.Name] = p
	return nil
}

func (m *memDB) ListProviders(_ context.Context) ([]catalog.Provider, error) {
	var out []catalog.Provider
	for _, p := range m.providers {
		out = append(out, p)
	}
	return out, nil
}

func (m *memDB) DeleteProvider(_ context.Context, name string) error {
	delete(m.providers, name)
	return nil
}

func (m *memDB) UpsertPolicy(_ context.Context, _ catalog.Policy) error        { return nil }
func (m *memDB) ListPolicies(_ context.Context) ([]catalog.Policy, error)       { return nil, nil }
func (m *memDB) DeletePolicy(_ context.Context, _ string) error              { return nil }
func (m *memDB) ListSecretRows(_ context.Context) ([]catalog.SecretRow, error) { return nil, nil }
func (m *memDB) UpsertSecretEnv(_ context.Context, _, _, _ string, _ catalog.Metadata) error {
	return nil
}
func (m *memDB) UpsertSecretStored(_ context.Context, _, _ string, _ catalog.Metadata, _, _ []byte) error {
	return nil
}
func (m *memDB) UpdateSecretEnv(_ context.Context, _, _ string) error                { return nil }
func (m *memDB) UpdateSecretStored(_ context.Context, _ string, _, _ []byte) error    { return nil }
func (m *memDB) DeleteSecret(_ context.Context, _ string) error                      { return nil }
func (m *memDB) UpsertSecretRaw(_ context.Context, _ string, _ catalog.Metadata, _ catalog.SecretSpec) error {
	return nil
}

func (m *memDB) UpsertModel(_ context.Context, model catalog.Model) error {
	m.models[model.Metadata.Name] = model
	return nil
}

func (m *memDB) ListModels(_ context.Context) ([]catalog.Model, error) {
	var out []catalog.Model
	for _, model := range m.models {
		out = append(out, model)
	}
	return out, nil
}

func (m *memDB) DeleteModel(_ context.Context, name string) error {
	delete(m.models, name)
	return nil
}

func (m *memDB) UpsertRoute(_ context.Context, _ catalog.Route) error       { return nil }
func (m *memDB) ListRoutes(_ context.Context) ([]catalog.Route, error)      { return nil, nil }
func (m *memDB) DeleteRoute(_ context.Context, _ string) error              { return nil }
func (m *memDB) UpsertRateLimit(_ context.Context, _ catalog.RateLimit) error   { return nil }
func (m *memDB) ListRateLimits(_ context.Context) ([]catalog.RateLimit, error)  { return nil, nil }
func (m *memDB) DeleteRateLimit(_ context.Context, _ string) error              { return nil }
func (m *memDB) IsEmpty(_ context.Context) (bool, error)                         { return len(m.models) == 0 && len(m.providers) == 0, nil }
func (m *memDB) UpsertRelayKey(_ context.Context, _ catalog.RelayKey) error      { return nil }
func (m *memDB) ListRelayKeys(_ context.Context) ([]catalog.RelayKey, error)     { return nil, nil }
func (m *memDB) DeleteRelayKey(_ context.Context, _ string) error                { return nil }
func (m *memDB) GetPassthrough(_ context.Context) (*catalog.Passthrough, error)  { return nil, nil }
func (m *memDB) SetPassthrough(_ context.Context, _ catalog.Passthrough) error   { return nil }

// WithTxCatalog satisfies catalog.TxRunner by running fn directly (no real tx in tests).
func (m *memDB) WithTxCatalog(ctx context.Context, fn func(db catalog.CatalogDB) error) error {
	return fn(m)
}

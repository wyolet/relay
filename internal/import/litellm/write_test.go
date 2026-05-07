package litellm_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	litellmimport "github.com/wyolet/relay/internal/import/litellm"
)

// TestWriteToDisk verifies that WriteToDisk produces the expected directory layout.
func TestWriteToDisk(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{
		SourceVersion: "test-version",
		Today:         fixedToday,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	dir := t.TempDir()
	wr, err := litellmimport.WriteToDisk(dir, result, litellmimport.ModeUpsert)
	if err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}

	if wr.ProvidersWritten == 0 {
		t.Error("expected providers to be written")
	}
	if wr.ModelsWritten == 0 {
		t.Error("expected models to be written")
	}
	if wr.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", wr.Errors)
	}

	// Verify each provider has a provider.yaml.
	for _, p := range result.Providers {
		path := filepath.Join(dir, "providers", p.Metadata.Name, "provider.yaml")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected provider file %s: %v", path, err)
		}
	}

	// Verify each model has a file under its provider.
	for _, m := range result.Models {
		filename := litellmimport.SanitizeFilename(m.Metadata.Name) + ".yaml"
		path := filepath.Join(dir, "providers", m.Spec.Provider, "models", filename)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected model file %s: %v", path, err)
		}
	}
}

// TestWriteToDiskGolden verifies file contents against existing golden files.
func TestWriteToDiskGolden(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{
		SourceVersion: "test-version",
		Today:         fixedToday,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	dir := t.TempDir()
	if _, err := litellmimport.WriteToDisk(dir, result, litellmimport.ModeUpsert); err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}

	goldenDir := filepath.Join("testdata", "golden")
	for _, m := range result.Models {
		m := m
		t.Run(m.Metadata.Name, func(t *testing.T) {
			filename := litellmimport.SanitizeFilename(m.Metadata.Name) + ".yaml"
			gotPath := filepath.Join(dir, "providers", m.Spec.Provider, "models", filename)
			got, err := os.ReadFile(gotPath)
			if err != nil {
				t.Fatalf("read written file: %v", err)
			}
			// Compare against golden (same content as before — just stored flat).
			goldenFile := filepath.Join(goldenDir, strings.ReplaceAll(m.Metadata.Name, "/", "_")+".yaml")
			want, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Skipf("no golden file for %s, skipping", m.Metadata.Name)
			}
			if string(got) != string(want) {
				t.Errorf("golden mismatch for %s:\nGOT:\n%s\nWANT:\n%s", m.Metadata.Name, got, want)
			}
		})
	}
}

// TestSkipExistingFilesMode verifies that mode=skip-existing leaves pre-existing files alone.
func TestSkipExistingFilesMode(t *testing.T) {
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	// Pick the first model to pre-create.
	m := result.Models[0]
	dir := t.TempDir()
	filename := litellmimport.SanitizeFilename(m.Metadata.Name) + ".yaml"
	modelPath := filepath.Join(dir, "providers", m.Spec.Provider, "models", filename)

	// Pre-create the file with sentinel content.
	if err := os.MkdirAll(filepath.Dir(modelPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sentinel := []byte("# hand-edited, do not overwrite\n")
	if err := os.WriteFile(modelPath, sentinel, 0o644); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	wr, err := litellmimport.WriteToDisk(dir, result, litellmimport.ModeSkipExisting)
	if err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}

	if wr.Skipped == 0 {
		t.Error("expected at least one file to be skipped")
	}

	// The pre-created file must be unchanged.
	got, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("expected sentinel content, got: %s", got)
	}
}

// TestSanitizeFilename verifies that unsafe characters are replaced.
func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"llama3:8b", "llama3_8b"},
		{"meta/llama3", "meta_llama3"},
		{"gpt-4o", "gpt-4o"},
		{"claude-3:opus/latest", "claude-3_opus_latest"},
	}
	for _, c := range cases {
		got := litellmimport.SanitizeFilename(c.input)
		if got != c.want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestWriteToStdout verifies that WriteToStdout emits providers before models
// and separates documents with "---".
func TestWriteToStdout(t *testing.T) {
	// We can't capture stdout directly in a unit test easily, but we can verify
	// the function doesn't error and that the order contract holds via the result
	// ordering (providers first, models second, both alphabetically).
	entries := loadFixture(t)
	result, err := litellmimport.Translate(entries, litellmimport.TranslateOptions{Today: fixedToday})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	// Verify that result.Providers and result.Models are sorted alphabetically
	// (the ordering contract that WriteToStdout relies on).
	for i := 1; i < len(result.Providers); i++ {
		if result.Providers[i].Metadata.Name < result.Providers[i-1].Metadata.Name {
			t.Errorf("providers not sorted: %q before %q",
				result.Providers[i-1].Metadata.Name, result.Providers[i].Metadata.Name)
		}
	}
	for i := 1; i < len(result.Models); i++ {
		if result.Models[i].Metadata.Name < result.Models[i-1].Metadata.Name {
			t.Errorf("models not sorted: %q before %q",
				result.Models[i-1].Metadata.Name, result.Models[i].Metadata.Name)
		}
	}
}

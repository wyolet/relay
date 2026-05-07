package litellm

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/internal/catalog"
)

// WriteResult summarises what happened during a write-to-disk pass.
type WriteResult struct {
	ProvidersWritten int
	ModelsWritten    int
	Skipped          int // skipped due to mode=skip-existing
	Errors           int
}

// SanitizeFilename replaces characters that are unsafe in filenames.
// ':' and '/' are replaced with '_' so that names like "llama3:8b" become "llama3_8b".
func SanitizeFilename(name string) string {
	r := strings.NewReplacer(":", "_", "/", "_")
	return r.Replace(name)
}

// WriteToDisk writes translated entities as YAML files under outDir, following the layout:
//
//	<outDir>/providers/<provider-name>/provider.yaml
//	<outDir>/providers/<provider-name>/models/<model-name>.yaml
//
// The mode parameter controls how existing files are handled (same semantics as ApplyMode):
//   - ModeUpsert / ModeOverwrite: overwrite any existing file.
//   - ModeSkipExisting: skip files that already exist on disk.
func WriteToDisk(outDir string, result *TranslateResult, mode ApplyMode) (*WriteResult, error) {
	if mode == "" {
		mode = ModeUpsert
	}
	wr := &WriteResult{}

	// Write provider files.
	for _, p := range result.Providers {
		path := filepath.Join(outDir, "providers", p.Metadata.Name, "provider.yaml")
		wrote, err := writeEntity(path, p, mode)
		if err != nil {
			slog.Error("import litellm: write provider failed", "path", path, "err", err)
			wr.Errors++
			continue
		}
		if wrote {
			slog.Info("import litellm: writing "+path)
			wr.ProvidersWritten++
		} else {
			slog.Info("import litellm: skipping existing "+path)
			wr.Skipped++
		}
	}

	// Write model files.
	for _, m := range result.Models {
		providerName := m.Spec.Provider
		filename := SanitizeFilename(m.Metadata.Name) + ".yaml"
		path := filepath.Join(outDir, "providers", providerName, "models", filename)
		wrote, err := writeEntity(path, m, mode)
		if err != nil {
			slog.Error("import litellm: write model failed", "path", path, "err", err)
			wr.Errors++
			continue
		}
		if wrote {
			slog.Info("import litellm: writing " + path)
			wr.ModelsWritten++
		} else {
			slog.Info("import litellm: skipping existing " + path)
			wr.Skipped++
		}
	}

	return wr, nil
}

// writeEntity marshals v as YAML and writes it to path, respecting mode.
// Returns (true, nil) if written, (false, nil) if skipped, (false, err) on error.
func writeEntity(path string, v any, mode ApplyMode) (bool, error) {
	if mode == ModeSkipExisting {
		if _, err := os.Stat(path); err == nil {
			// File exists — skip.
			return false, nil
		}
	}

	b, err := yaml.Marshal(v)
	if err != nil {
		return false, fmt.Errorf("marshal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("mkdir: %w", err)
	}

	if err := os.WriteFile(path, b, 0o644); err != nil {
		return false, fmt.Errorf("write file: %w", err)
	}
	return true, nil
}

// WriteToStdout writes all providers then all models (alphabetically) to w as
// YAML documents separated by "---\n".  The caller is responsible for closing w.
func WriteToStdout(result *TranslateResult) error {
	// Providers first (already sorted by Translate), then models.
	for _, p := range result.Providers {
		b, err := yaml.Marshal(p)
		if err != nil {
			return fmt.Errorf("marshal provider %s: %w", p.Metadata.Name, err)
		}
		fmt.Printf("---\n%s", b)
	}
	for _, m := range result.Models {
		b, err := yaml.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal model %s: %w", m.Metadata.Name, err)
		}
		fmt.Printf("---\n%s", b)
	}
	return nil
}

// Ensure catalog types compile (import used).
var _ = catalog.Model{}

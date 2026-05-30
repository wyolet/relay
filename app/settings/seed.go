package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SeedDir loads settings-section YAML from dir and upserts each section that
// has NO existing DB row — **seed-if-absent**. It never clobbers a runtime
// change or a prior seed, so the bootstrap tool's live `PUT /settings/...`
// stays authoritative. This is the first-boot / airgapped path; managed
// deployments are configured at runtime via the settings API.
//
// Each file is named `<section>.yaml` (e.g. `usage-logging.yaml`) and its
// body is the section value. Unknown sections and invalid values are hard
// errors (fail fast on a typo). A missing dir is a no-op, not an error.
// Returns the section names actually seeded, lexically sorted.
func SeedDir(ctx context.Context, store *Store, dir string) ([]string, error) {
	files, err := loadSectionFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}

	existing, err := store.existingSections(ctx)
	if err != nil {
		return nil, err
	}

	var seeded []string
	for section, raw := range files {
		if existing[section] {
			continue // already configured — don't clobber
		}
		if _, err := store.Upsert(ctx, section, raw); err != nil {
			return nil, fmt.Errorf("settings.SeedDir %s: %w", section, err)
		}
		seeded = append(seeded, section)
	}
	sort.Strings(seeded)
	return seeded, nil
}

// loadSectionFiles reads <section>.yaml/.yml files from dir, validates each
// names a registered section, and converts each body to JSON the section's
// Decode can parse. Pure (filesystem + registry, no DB) so it's unit-testable.
// A missing dir yields an empty map.
func loadSectionFiles(dir string) (map[string]json.RawMessage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("settings.loadSectionFiles: %w", err)
	}

	out := map[string]json.RawMessage{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		section := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if _, ok := Lookup(section); !ok {
			return nil, fmt.Errorf("settings.SeedDir: %s names unknown section %q", e.Name(), section)
		}
		raw, err := yamlFileToJSON(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("settings.SeedDir %s: %w", e.Name(), err)
		}
		out[section] = raw
	}
	return out, nil
}

// yamlFileToJSON reads a YAML file and converts its mapping to JSON bytes.
// yaml.v3 decodes mappings into map[string]any, so json.Marshal round-trips
// cleanly into the section's JSON-tagged struct.
func yamlFileToJSON(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("to json: %w", err)
	}
	return j, nil
}

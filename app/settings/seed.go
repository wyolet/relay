package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/wyolet/relay/app/manifest"
)

// SeedDir loads settings from manifest YAML in dir and upserts each section
// that has NO existing DB row — **seed-if-absent**. It never clobbers a
// runtime change or a prior seed, so a live `PUT /settings/...` stays
// authoritative. This is the first-boot / airgapped path; managed deployments
// are configured at runtime via the settings API.
//
// Settings use the same Kubernetes-style manifest envelope as every other
// catalog resource — `apiVersion` + `kind: Setting` + `metadata.name` (the
// section key) + `spec` (the section's typed value). Files may hold multiple
// `---`-separated docs. metadata.name selecting an unregistered section, a
// spec that fails the section's Decode, or a non-Setting kind in the tree are
// all hard errors (fail fast on a typo). A missing dir is a no-op.
//
// Returns the section names actually seeded, lexically sorted.
func SeedDir(ctx context.Context, store *Store, dir string) ([]string, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("settings.SeedDir: %w", err)
	}
	sections, err := sectionsFromDocs(docs)
	if err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, nil
	}

	existing, err := store.existingSections(ctx)
	if err != nil {
		return nil, err
	}

	var seeded []string
	for section, raw := range sections {
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

// sectionsFromDocs extracts Setting docs into a section→spec-JSON map. Every
// doc must be a Setting naming a registered section; anything else is an
// error (the settings tree is settings-only — a stray kind is a misplaced
// file, not something to skip silently). Pure (registry only, no DB) so it's
// unit-testable.
func sectionsFromDocs(docs []manifest.Document) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(docs))
	for _, d := range docs {
		if d.Setting == nil {
			return nil, fmt.Errorf("settings.SeedDir: unexpected kind %q (settings tree accepts only kind: Setting)", d.Kind())
		}
		section := d.Setting.Metadata.Name
		if _, ok := Lookup(section); !ok {
			return nil, fmt.Errorf("settings.SeedDir: %q names unknown settings section", section)
		}
		raw, err := d.Setting.SpecJSON()
		if err != nil {
			return nil, fmt.Errorf("settings.SeedDir %s: spec: %w", section, err)
		}
		// Validate the spec against the section's typed value now, so a bad
		// manifest fails at load rather than at Upsert.
		sec, _ := Lookup(section)
		if _, err := sec.Decode(raw); err != nil {
			return nil, fmt.Errorf("settings.SeedDir %s: %w", section, err)
		}
		out[section] = raw
	}
	return out, nil
}

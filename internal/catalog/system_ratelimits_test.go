package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSystemRateLimitsLoad verifies that the four system-mirrored RateLimit
// YAML files in config/ratelimits/system/ load and validate correctly.
// The test locates the config directory relative to the module root by walking
// up from the package source tree.
func TestSystemRateLimitsLoad(t *testing.T) {
	// Walk up to find the repo root (the directory containing go.mod).
	repoRoot := findRepoRoot(t)
	systemDir := filepath.Join(repoRoot, "config", "ratelimits", "system")

	if _, err := os.Stat(systemDir); os.IsNotExist(err) {
		t.Fatalf("config/ratelimits/system/ not found under repo root %s", repoRoot)
	}

	// LoadYAML validates all cross-entity constraints, but our system RL
	// directory only contains RateLimits — no providers. We bypass the full
	// catalog validator by loading each file into a bare snapshot directly.
	snap := newSnapshot()
	entries, err := os.ReadDir(systemDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(systemDir, e.Name())
		if err := loadFile(path, snap); err != nil {
			t.Fatalf("loadFile(%s): %v", e.Name(), err)
		}
	}

	// Validate just the rate-limits (not the full snapshot — no providers present).
	if err := validateRateLimits(snap); err != nil {
		t.Fatalf("validateRateLimits: %v", err)
	}

	wantNames := []string{"system-api", "inference", "inference-proxy", "inference-proxy-anonymous"}
	for _, name := range wantNames {
		rl, ok := snap.rateLimits[name]
		if !ok {
			t.Errorf("RateLimit %q not found in snapshot", name)
			continue
		}
		if rl.Metadata.Owner.Kind != OwnerSystem {
			t.Errorf("RateLimit %q: want owner.kind=%q, got %q",
				name, OwnerSystem, rl.Metadata.Owner.Kind)
		}
	}
	if t.Failed() {
		t.Logf("loaded rate limits: %v", snapRLNames(snap))
	}
}

func snapRLNames(s *snapshot) []string {
	names := make([]string, 0, len(s.rateLimits))
	for n := range s.rateLimits {
		names = append(names, n)
	}
	return names
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Start at the package directory and walk up looking for go.mod.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod)")
		}
		dir = parent
	}
}

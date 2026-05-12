package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSystemRateLimitsLoad verifies that the four system-owned RateLimit
// documents bundled in config/ratelimits/system.yaml load and validate.
func TestSystemRateLimitsLoad(t *testing.T) {
	repoRoot := findRepoRoot(t)
	systemFile := filepath.Join(repoRoot, "config", "ratelimits", "system.yaml")

	if _, err := os.Stat(systemFile); os.IsNotExist(err) {
		t.Fatalf("config/ratelimits/system.yaml not found under repo root %s", repoRoot)
	}

	snap := newSnapshot()
	if err := loadFile(systemFile, snap); err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	// Validate just the rate-limits (not the full snapshot — no providers present).
	if err := validateRateLimits(snap); err != nil {
		t.Fatalf("validateRateLimits: %v", err)
	}

	wantNames := []string{"control-api", "inference-api", "inference-api-proxy", "inference-api-proxy-anonymous"}
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

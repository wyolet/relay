package keypool

import (
	"regexp"
	"testing"
)

// hashTagRE matches a Redis key that contains exactly one valid hash tag:
// no curly braces outside the tag, one {...} segment with no nested braces.
var hashTagRE = regexp.MustCompile(`^[^{}]*\{[^{}]+\}[^{}]*$`)

func TestCircuitKey_HashTag(t *testing.T) {
	key := circuitKey("sha256abc123def456")

	// Sample: secret_health:{secret:sha256abc123def456}
	if !hashTagRE.MatchString(key) {
		t.Errorf("circuitKey does not match hash-tag pattern: %q", key)
	}
}

func TestRoundRobinKey_HashTag(t *testing.T) {
	key := roundRobinKey("prod-pool")

	// Sample: pool_rr:{pool:prod-pool}
	if !hashTagRE.MatchString(key) {
		t.Errorf("roundRobinKey does not match hash-tag pattern: %q", key)
	}
}

func TestLRUKey_HashTag(t *testing.T) {
	key := lruKey("0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e", "sha256abc123def456")

	// Sample: {pool_lru:0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e}:sha256abc123def456
	if !hashTagRE.MatchString(key) || key != "{pool_lru:0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e}:sha256abc123def456" {
		t.Errorf("lruKey = %q", key)
	}
}

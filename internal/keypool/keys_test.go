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
